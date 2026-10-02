package vercel

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/errkind"
)

var logsNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func msText(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

func logLine(id string, at time.Time, status int) string {
	return `{"id":"` + id + `","timestamp":` + msText(at) + `,"level":"info","source":"serverless","requestMethod":"GET","requestPath":"/p","responseStatusCode":` + strconv.Itoa(status) + `,"message":"m","logs":[]}`
}

// A full page asks the newest window, joins every value to its flag, keeps
// what each request printed, and names its oldest entry as the cursor.
func TestReadLogs(t *testing.T) {
	run := func(_ context.Context, args []string) ([]byte, error) {
		want := []string{"logs", "--project=prj_1", "--scope=team_a",
			"--since=2026-10-01T11:00:00Z", "--until=2026-10-01T12:00:00Z", "--limit=3",
			"--environment=production", "--status-code=5xx", "--json", "--non-interactive"}
		if !reflect.DeepEqual(args, want) {
			t.Fatalf("args:\n%v\nwant\n%v", args, want)
		}
		return []byte(`Fetching logs...
{"id":"a","timestamp":1000,"level":"error","source":"serverless","requestMethod":"GET","requestPath":"/api/x","responseStatusCode":500,"message":"boom","logs":[{"level":"error","message":"boom"},{"level":"info","message":"after"}]}
{"id":"b","timestamp":2000,"level":"info","source":"static","requestMethod":"GET","requestPath":"/","responseStatusCode":503,"message":"Service unavailable","logs":[]}
{"id":"c","timestamp":500,"level":"info","source":"static","requestMethod":"GET","requestPath":"/","responseStatusCode":503,"message":"x","logs":[]}
`), nil
	}
	got, err := readLogsWith(t.Context(), LogQuery{TeamID: "team_a", ProjectID: "prj_1", Environment: "production", StatusCode: "5xx", Limit: 2}, logsNow, run)
	want := LogPage{NextCursor: "1000:a", Entries: []LogEntry{
		{ID: "b", At: time.UnixMilli(2000), Level: "info", Source: "static", Method: "GET", Path: "/", Status: 503,
			Lines: []LogLine{{Level: "info", Message: "Service unavailable"}}},
		{ID: "a", At: time.UnixMilli(1000), Level: "error", Source: "serverless", Method: "GET", Path: "/api/x", Status: 500,
			Lines: []LogLine{{Level: "error", Message: "boom"}, {Level: "info", Message: "after"}}},
	}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, %v", got, err)
	}
}

// The next page ends at the cursor's entry and leaves it out: `--until`
// includes it again.
func TestReadLogsSkipsTheCursorEntry(t *testing.T) {
	cursorAt := logsNow.Add(-10 * time.Minute)
	var args []string
	run := func(_ context.Context, a []string) ([]byte, error) {
		args = a
		return []byte(logLine("a-1", cursorAt, 500) + "\n" + logLine("b", cursorAt.Add(-time.Second), 500) + "\n"), nil
	}
	got, err := readLogsWith(t.Context(), LogQuery{TeamID: "team_a", ProjectID: "prj_1", Cursor: msText(cursorAt) + ":a-1", Limit: 1}, logsNow, run)
	if err != nil || len(got.Entries) != 1 || got.Entries[0].ID != "b" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if !slices.Contains(args, "--until=2026-10-01T11:50:00Z") || !slices.Contains(args, "--limit=2") {
		t.Fatalf("args %v", args)
	}
}

// A short page has drained its window, so its cursor is that window's
// start; quiet windows before it are passed over, up to three per page.
func TestReadLogsWalksBackPastQuietWindows(t *testing.T) {
	var untils []string
	run := func(_ context.Context, a []string) ([]byte, error) {
		for _, arg := range a {
			if strings.HasPrefix(arg, "--until=") {
				untils = append(untils, strings.TrimPrefix(arg, "--until="))
			}
		}
		if len(untils) == 2 {
			return []byte(logLine("x", logsNow.Add(-90*time.Minute), 500) + "\n"), nil
		}
		return nil, nil
	}
	got, err := readLogsWith(t.Context(), LogQuery{TeamID: "team_a", ProjectID: "prj_1", Limit: 10}, logsNow, run)
	if err != nil || len(got.Entries) != 1 || got.Entries[0].ID != "x" {
		t.Fatalf("got %+v, %v", got, err)
	}
	wantUntils := []string{"2026-10-01T12:00:00Z", "2026-10-01T10:59:59.999Z"}
	if !reflect.DeepEqual(untils, wantUntils) {
		t.Fatalf("untils %v", untils)
	}
	if want := msText(logsNow.Add(-2*time.Hour - 2*time.Millisecond)); got.NextCursor != want {
		t.Fatalf("cursor %q, want %q", got.NextCursor, want)
	}

	untils = nil
	empty, err := readLogsWith(t.Context(), LogQuery{TeamID: "team_a", ProjectID: "prj_1"}, logsNow, func(_ context.Context, a []string) ([]byte, error) {
		untils = append(untils, a[4])
		return nil, nil
	})
	if err != nil || len(empty.Entries) != 0 || len(untils) != windowsPerPage || empty.NextCursor == "" {
		t.Fatalf("quiet: %+v after %d windows, %v", empty, len(untils), err)
	}
}

// Paging ends at the start of Vercel's history, and the window and limit are capped.
func TestReadLogsStopsAtHistory(t *testing.T) {
	var args []string
	run := func(_ context.Context, a []string) ([]byte, error) { args = a; return nil, nil }
	cursor := msText(logsNow.Add(-20 * time.Hour))
	got, err := readLogsWith(t.Context(), LogQuery{TeamID: "team_a", ProjectID: "prj_1", Window: 48 * time.Hour, Cursor: cursor, Limit: 1000}, logsNow, run)
	if err != nil || got.NextCursor != "" || got.Entries == nil {
		t.Fatalf("got %+v, %v", got, err)
	}
	if !slices.Contains(args, "--since=2026-09-30T12:00:00Z") || !slices.Contains(args, "--limit=201") {
		t.Fatalf("args %v", args)
	}
}

// A long line is cut and a chatty request keeps its first lines.
func TestReadLogsTrimsLines(t *testing.T) {
	logs := make([]LogLine, 30)
	for i := range logs {
		logs[i] = LogLine{Level: "info", Message: "x"}
	}
	logs[0].Message = strings.Repeat("y", 3000)
	got := logLines(logs, "info", "")
	if len(got) != maxLogLines || len([]rune(got[0].Message)) != maxLineLength || !strings.HasSuffix(got[0].Message, "…") {
		t.Fatalf("got %d lines, first %d runes", len(got), len([]rune(got[0].Message)))
	}
}

// Nothing reaches the CLI that could be read as a flag or an unknown filter.
func TestReadLogsRefusesBadQueries(t *testing.T) {
	run := func(context.Context, []string) ([]byte, error) { t.Fatal("ran the CLI"); return nil, nil }
	for _, q := range []LogQuery{
		{ProjectID: "prj_1"},
		{TeamID: "team_a", ProjectID: "--token=x"},
		{TeamID: "team_a", ProjectID: "prj_1", Environment: "staging"},
		{TeamID: "team_a", ProjectID: "prj_1", Level: "debug"},
		{TeamID: "team_a", ProjectID: "prj_1", StatusCode: "5xx --token"},
		{TeamID: "team_a", ProjectID: "prj_1", Cursor: "yesterday"},
	} {
		if _, err := readLogsWith(t.Context(), q, logsNow, run); !errors.Is(err, errkind.ErrInvalid) {
			t.Fatalf("%+v: err = %v", q, err)
		}
	}
}
