package vercel

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/repogo/host/internal/errkind"
)

const (
	defaultLogLimit  = 100
	maxLogLimit      = 200
	defaultLogWindow = time.Hour
	maxLogWindow     = 24 * time.Hour

	// Vercel kept a day of request logs in a 7-day query (10-01), so paging
	// stops there.
	logHistory = 24 * time.Hour
	// A quiet hour gives an empty window; a page looks this many back before
	// it answers with nothing.
	windowsPerPage = 3

	// A request can print thousands of lines; a page shows the first few of each.
	maxLogLines   = 20
	maxLineLength = 2000
)

var (
	vercelID   = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	statusCode = regexp.MustCompile(`^[1-5]([0-9]{2}|xx)$`)
)

// LogQuery is one page of a project's request logs, newest first. Cursor is
// the last page's NextCursor, or "" for the newest.
type LogQuery struct {
	TeamID      string
	ProjectID   string
	Environment string // production, preview, or "" for both
	Level       string // error, warning, info, fatal, or ""
	StatusCode  string // 500, 5xx, or ""
	Window      time.Duration
	Cursor      string
	Limit       int
}

// LogEntry is one request and what it printed.
type LogEntry struct {
	ID     string    `json:"id"`
	At     time.Time `json:"at"`
	Level  string    `json:"level"`
	Source string    `json:"source"`
	Method string    `json:"method"`
	Path   string    `json:"path"`
	Status int       `json:"status"`
	Lines  []LogLine `json:"lines" wire:"array"`
}

type LogLine struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// LogPage is a page of entries; NextCursor is "" once paging reaches the
// start of Vercel's history.
type LogPage struct {
	Entries    []LogEntry `json:"entries" wire:"array"`
	NextCursor string     `json:"next_cursor"`
}

// logCursor is where a page ends: the oldest entry it returned, which the
// next page skips because `--until` includes it, or a window's start (ID "").
type logCursor struct {
	until time.Time
	id    string
}

func parseLogCursor(s string) (logCursor, error) {
	ms, id, _ := strings.Cut(s, ":")
	n, err := strconv.ParseInt(ms, 10, 64)
	// The ID is only compared with entries, never passed to the CLI.
	if err != nil {
		return logCursor{}, fmt.Errorf("%w: cursor is a next_cursor from an earlier page", errkind.ErrInvalid)
	}
	return logCursor{until: time.UnixMilli(n), id: id}, nil
}

func (c logCursor) String() string {
	s := strconv.FormatInt(c.until.UnixMilli(), 10)
	if c.id != "" {
		s += ":" + c.id
	}
	return s
}

// ReadLogs reads one page of a project's request logs with `vercel logs`.
func ReadLogs(ctx context.Context, q LogQuery) (LogPage, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	return readLogsWith(ctx, q, time.Now(), runCLI)
}

// readLogsWith reads windows, newest first, until one has entries. Every call
// has a window: without one the CLI scans back for 13 s or more.
func readLogsWith(ctx context.Context, q LogQuery, now time.Time, run func(context.Context, []string) ([]byte, error)) (LogPage, error) {
	if err := checkLogQuery(q); err != nil {
		return LogPage{}, err
	}
	cursor := logCursor{until: now}
	if q.Cursor != "" {
		var err error
		if cursor, err = parseLogCursor(q.Cursor); err != nil {
			return LogPage{}, err
		}
		if cursor.until.After(now) {
			cursor.until = now
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultLogLimit
	}
	limit = min(limit, maxLogLimit)
	window := q.Window
	if window <= 0 {
		window = defaultLogWindow
	}
	window = min(window, maxLogWindow)
	oldest := now.Add(-logHistory)

	page := LogPage{Entries: []LogEntry{}}
	for range windowsPerPage {
		if !cursor.until.After(oldest) {
			return page, nil
		}
		since := cursor.until.Add(-window)
		if since.Before(oldest) {
			since = oldest
		}
		// One extra, because the entry the cursor names comes back again.
		out, err := run(ctx, logArgs(q, since, cursor.until, limit+1))
		if err != nil {
			return LogPage{}, err
		}
		entries, err := parseLogs(out)
		if err != nil {
			return LogPage{}, err
		}
		entries = slices.DeleteFunc(entries, func(e LogEntry) bool { return cursor.id != "" && e.ID == cursor.id })
		if len(entries) >= limit {
			page.Entries = entries[:limit]
			last := page.Entries[limit-1]
			page.NextCursor = logCursor{until: last.At, id: last.ID}.String()
			return page, nil
		}
		// The window is exhausted. The next starts a millisecond before its
		// start, which `--since` included.
		cursor = logCursor{until: since.Add(-time.Millisecond)}
		if len(entries) > 0 {
			page.Entries = entries
			break
		}
	}
	if cursor.until.After(oldest) {
		page.NextCursor = cursor.String()
	}
	return page, nil
}

func checkLogQuery(q LogQuery) error {
	if !vercelID.MatchString(q.TeamID) || !vercelID.MatchString(q.ProjectID) {
		return fmt.Errorf("%w: team_id and project_id are required", errkind.ErrInvalid)
	}
	if !slices.Contains([]string{"", "production", "preview"}, q.Environment) {
		return fmt.Errorf("%w: environment is production or preview", errkind.ErrInvalid)
	}
	if !slices.Contains([]string{"", "error", "warning", "info", "fatal"}, q.Level) {
		return fmt.Errorf("%w: level is error, warning, info or fatal", errkind.ErrInvalid)
	}
	if q.StatusCode != "" && !statusCode.MatchString(q.StatusCode) {
		return fmt.Errorf("%w: status_code is like 500 or 5xx", errkind.ErrInvalid)
	}
	return nil
}

// logArgs joins every value to its flag so none can be read as a flag of its own.
func logArgs(q LogQuery, since, until time.Time, limit int) []string {
	args := []string{"logs", "--project=" + q.ProjectID, "--scope=" + q.TeamID,
		"--since=" + since.UTC().Format(time.RFC3339Nano),
		"--until=" + until.UTC().Format(time.RFC3339Nano),
		"--limit=" + strconv.Itoa(limit)}
	if q.Environment != "" {
		args = append(args, "--environment="+q.Environment)
	}
	if q.Level != "" {
		args = append(args, "--level="+q.Level)
	}
	if q.StatusCode != "" {
		args = append(args, "--status-code="+q.StatusCode)
	}
	return append(args, "--json", "--non-interactive")
}

// parseLogs reads the CLI's JSON lines, newest first.
func parseLogs(out []byte) ([]LogEntry, error) {
	entries := []LogEntry{}
	lines := bufio.NewScanner(bytes.NewReader(out))
	lines.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for lines.Scan() {
		var raw struct {
			ID        string    `json:"id"`
			Timestamp int64     `json:"timestamp"`
			Level     string    `json:"level"`
			Source    string    `json:"source"`
			Method    string    `json:"requestMethod"`
			Path      string    `json:"requestPath"`
			Status    int       `json:"responseStatusCode"`
			Message   string    `json:"message"`
			Logs      []LogLine `json:"logs"`
		}
		// The CLI prints its progress ("Fetching logs...") on the same stream.
		if json.Unmarshal(lines.Bytes(), &raw) != nil || raw.ID == "" {
			continue
		}
		entries = append(entries, LogEntry{
			ID: raw.ID, At: time.UnixMilli(raw.Timestamp), Level: raw.Level, Source: raw.Source,
			Method: raw.Method, Path: raw.Path, Status: raw.Status, Lines: logLines(raw.Logs, raw.Level, raw.Message),
		})
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("read logs: %w", err)
	}
	slices.SortStableFunc(entries, func(a, b LogEntry) int { return b.At.Compare(a.At) })
	return entries, nil
}

// logLines is what the request printed, or Vercel's one-line message when it
// printed nothing; Vercel repeats the first printed line as the message.
func logLines(logs []LogLine, level, message string) []LogLine {
	if len(logs) == 0 && message != "" {
		logs = []LogLine{{Level: level, Message: message}}
	}
	out := make([]LogLine, 0, min(len(logs), maxLogLines))
	for _, line := range logs[:min(len(logs), maxLogLines)] {
		if r := []rune(line.Message); len(r) > maxLineLength {
			line.Message = string(r[:maxLineLength-1]) + "…"
		}
		out = append(out, line)
	}
	return out
}
