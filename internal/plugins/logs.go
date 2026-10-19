package plugins

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/sqlitedb"
)

// Every console line a plugin writes lands here, with the call it was written
// in, so an agent can read what its plugin did; the host trims the file by
// age and size, oldest lines of the noisiest plugin first.

// LogLine is one console line a plugin wrote, as plugins.logs answers it.
type LogLine struct {
	ID int64 `json:"id"`
	// At is ms since the epoch.
	At     int64  `json:"at"`
	Plugin string `json:"plugin"`
	// Dir is the plugins folder the plugin ran from: the project's or the environment's.
	Dir   string `json:"dir"`
	Level string `json:"level"`
	// Method is `<plugin>.<method>` of the run the line was written in; empty outside one.
	Method string `json:"method"`
	Caller string `json:"caller"`
	// CallID is one id per outermost call, so a call's lines read together.
	CallID string `json:"call_id"`
	// ChatID is the chat whose agent made the call, for an agent's.
	ChatID string          `json:"chat_id"`
	Text   string          `json:"text"`
	Data   json.RawMessage `json:"data"`
}

// logRecord is a line as the plugins server sends it, in the SDK's own names.
type logRecord struct {
	At     int64           `json:"at"`
	Plugin string          `json:"plugin"`
	Level  string          `json:"level"`
	Method string          `json:"method"`
	Caller string          `json:"caller"`
	CallID string          `json:"callId"`
	ChatID string          `json:"chatId"`
	Text   string          `json:"text"`
	Data   json.RawMessage `json:"data"`
}

// logPush is the server's `log` push: a folder's plugins' lines since the last.
type logPush struct {
	Dir     string      `json:"dir"`
	Records []logRecord `json:"records"`
}

// LogQuery narrows plugins.logs; every field may be zero.
type LogQuery struct {
	Plugin string
	// Level is that level and worse: debug, info, warn, error.
	Level  string
	Method string
	// Since and Until are ms since the epoch; Until excludes.
	Since, Until int64
	// Query is a substring of the text.
	Query string
	// Limit is at most logsLimitMax; zero is logsLimitDefault.
	Limit int
}

// LogLimits is how much the host keeps: lines newer than Keep, in a file
// under MaxBytes, checked every Every and at start.
type LogLimits struct {
	Keep     time.Duration
	MaxBytes int64
	Every    time.Duration
}

var defaultLogLimits = LogLimits{Keep: 7 * 24 * time.Hour, MaxBytes: 2 << 30, Every: 10 * time.Minute}

const (
	logsLimitDefault = 100
	logsLimitMax     = 1000
	// logQueueMax is how many lines wait for the writer; past it the oldest go.
	logQueueMax = 4096
	// purgeHour is how much of the noisiest plugin goes at a time while the file is too big.
	purgeHour = int64(time.Hour / time.Millisecond)
)

var levelRank = map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3}

const logsSchema = `
CREATE TABLE IF NOT EXISTS logs (
	id INTEGER PRIMARY KEY,
	at INTEGER NOT NULL,
	plugin TEXT NOT NULL,
	dir TEXT NOT NULL,
	level TEXT NOT NULL,
	method TEXT NOT NULL,
	caller TEXT NOT NULL,
	call_id TEXT NOT NULL,
	chat_id TEXT NOT NULL,
	text TEXT NOT NULL,
	data TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS logs_plugin_at ON logs(plugin, at);
CREATE INDEX IF NOT EXISTS logs_at ON logs(at);
`

// Logs is the plugin log: one SQLite file, one writer, trimmed on a timer.
type Logs struct {
	db     *sql.DB
	log    *slog.Logger
	limits LogLimits
	// wrote runs after each batch lands, naming the plugins in it.
	wrote func(plugins []string)

	mu      sync.Mutex
	queue   []LogLine
	dropped int
	wake    chan struct{}
	done    chan struct{}
}

// OpenLogs opens the log at path and starts its writer and purge under ctx;
// wrote may be nil. Lines are written in the order they arrive.
func OpenLogs(ctx context.Context, path string, limits LogLimits, log *slog.Logger, wrote func(plugins []string)) (*Logs, error) {
	// Incremental vacuum lets a purge give pages back without rewriting the file.
	db := sqlitedb.Open(path, "PRAGMA auto_vacuum=INCREMENTAL", "PRAGMA busy_timeout=5000", "PRAGMA synchronous=NORMAL")
	if _, err := db.Exec(logsSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("plugins: open the log at %s: %w", path, err)
	}
	l := &Logs{db: db, log: log, limits: limits, wrote: wrote, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go l.run(ctx)
	return l, nil
}

// Add queues a folder's lines. A full queue loses its oldest, and the next
// batch says how many.
func (l *Logs) Add(dir string, records []logRecord) {
	l.mu.Lock()
	for _, r := range records {
		data := r.Data
		if len(data) == 0 {
			data = json.RawMessage("[]")
		}
		l.queue = append(l.queue, LogLine{
			At: r.At, Plugin: r.Plugin, Dir: dir, Level: r.Level, Method: r.Method, Caller: r.Caller,
			CallID: r.CallID, ChatID: r.ChatID, Text: r.Text, Data: data,
		})
	}
	if over := len(l.queue) - logQueueMax; over > 0 {
		l.queue = append([]LogLine(nil), l.queue[over:]...)
		l.dropped += over
	}
	l.mu.Unlock()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

// Query answers the newest lines first, as LogQuery narrows them.
func (l *Logs) Query(ctx context.Context, q LogQuery) ([]LogLine, error) {
	where := []string{"1=1"}
	var args []any
	if q.Plugin != "" {
		where = append(where, "plugin = ?")
		args = append(args, q.Plugin)
	}
	if q.Level != "" {
		rank, ok := levelRank[q.Level]
		if !ok {
			return nil, errkind.ErrInvalid.Errorf("plugins: level is debug, info, warn or error, not %q", q.Level)
		}
		var levels []string
		for name, r := range levelRank {
			if r >= rank {
				levels = append(levels, "'"+name+"'")
			}
		}
		where = append(where, "level IN ("+strings.Join(levels, ",")+")")
	}
	if q.Method != "" {
		where = append(where, "method = ?")
		args = append(args, q.Method)
	}
	if q.Since > 0 {
		where = append(where, "at >= ?")
		args = append(args, q.Since)
	}
	if q.Until > 0 {
		where = append(where, "at < ?")
		args = append(args, q.Until)
	}
	if q.Query != "" {
		where = append(where, "instr(text, ?) > 0")
		args = append(args, q.Query)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = logsLimitDefault
	}
	limit = min(limit, logsLimitMax)
	args = append(args, limit)
	rows, err := l.db.QueryContext(ctx, `SELECT id, at, plugin, dir, level, method, caller, call_id, chat_id, text, data FROM logs WHERE `+
		strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lines := []LogLine{}
	for rows.Next() {
		var line LogLine
		var data string
		if err := rows.Scan(&line.ID, &line.At, &line.Plugin, &line.Dir, &line.Level, &line.Method, &line.Caller, &line.CallID, &line.ChatID, &line.Text, &data); err != nil {
			return nil, err
		}
		line.Data = json.RawMessage(data)
		lines = append(lines, line)
	}
	return lines, rows.Err()
}

// Errors counts each plugin's error lines since `since` (ms), keyed by its folder and name.
func (l *Logs) Errors(ctx context.Context, since int64) (map[string]int, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT dir, plugin, COUNT(*) FROM logs WHERE level = 'error' AND at >= ? GROUP BY dir, plugin`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var dir, plugin string
		var n int
		if err := rows.Scan(&dir, &plugin, &n); err != nil {
			return nil, err
		}
		out[errorsKey(dir, plugin)] = n
	}
	return out, rows.Err()
}

func errorsKey(dir, plugin string) string { return dir + "/" + plugin }

// Purge drops lines older than Keep, then the oldest hour of whichever plugin
// holds the most lines, until the file is under MaxBytes.
func (l *Logs) Purge(ctx context.Context, now time.Time) error {
	if _, err := l.db.ExecContext(ctx, `DELETE FROM logs WHERE at < ?`, now.Add(-l.limits.Keep).UnixMilli()); err != nil {
		return err
	}
	for {
		used, err := l.usedBytes(ctx)
		if err != nil || used <= l.limits.MaxBytes {
			return err
		}
		var dir, plugin string
		var oldest sql.NullInt64
		err = l.db.QueryRowContext(ctx, `SELECT dir, plugin, MIN(at) FROM logs GROUP BY dir, plugin ORDER BY COUNT(*) DESC LIMIT 1`).Scan(&dir, &plugin, &oldest)
		if err == sql.ErrNoRows || !oldest.Valid {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := l.db.ExecContext(ctx, `DELETE FROM logs WHERE dir = ? AND plugin = ? AND at < ?`, dir, plugin, oldest.Int64+purgeHour); err != nil {
			return err
		}
		if _, err := l.db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
			return err
		}
	}
}

// usedBytes is the file's pages in use: free pages are given back by the vacuum.
func (l *Logs) usedBytes(ctx context.Context) (int64, error) {
	var pages, free, size int64
	for name, into := range map[string]*int64{"page_count": &pages, "freelist_count": &free, "page_size": &size} {
		if err := l.db.QueryRowContext(ctx, "PRAGMA "+name).Scan(into); err != nil {
			return 0, err
		}
	}
	return (pages - free) * size, nil
}

// Close stops the writer once what it holds is written.
func (l *Logs) Close() {
	l.mu.Lock()
	if l.wake != nil {
		close(l.wake)
		l.wake = nil
	}
	l.mu.Unlock()
	<-l.done
	l.db.Close()
}

// run writes batches as they come and purges on the timer; ctx ending is the
// host going, after which Close finishes the queue.
func (l *Logs) run(ctx context.Context) {
	defer close(l.done)
	l.mu.Lock()
	wake := l.wake
	l.mu.Unlock()
	if err := l.Purge(ctx, time.Now()); err != nil {
		l.log.Warn("plugins: purging the log failed", "err", err)
	}
	ticker := time.NewTicker(l.limits.Every)
	defer ticker.Stop()
	for {
		select {
		case _, open := <-wake:
			l.flush(ctx)
			if !open {
				return
			}
		case <-ticker.C:
			if err := l.Purge(ctx, time.Now()); err != nil {
				l.log.Warn("plugins: purging the log failed", "err", err)
			}
		}
	}
}

// flush writes everything queued in one transaction and tells the host's log too.
func (l *Logs) flush(ctx context.Context) {
	l.mu.Lock()
	batch := l.queue
	dropped := l.dropped
	l.queue = nil
	l.dropped = 0
	l.mu.Unlock()
	if dropped > 0 {
		batch = append([]LogLine{{At: time.Now().UnixMilli(), Level: "warn", Data: json.RawMessage("[]"),
			Text: fmt.Sprintf("dropped %d lines: the log queue was full", dropped)}}, batch...)
	}
	if len(batch) == 0 {
		return
	}
	tx, err := l.db.Begin()
	if err != nil {
		l.log.Warn("plugins: writing the log failed", "err", err)
		return
	}
	for _, line := range batch {
		if _, err := tx.Exec(`INSERT INTO logs (at, plugin, dir, level, method, caller, call_id, chat_id, text, data) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			line.At, line.Plugin, line.Dir, line.Level, line.Method, line.Caller, line.CallID, line.ChatID, line.Text, string(line.Data)); err != nil {
			_ = tx.Rollback()
			l.log.Warn("plugins: writing the log failed", "err", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		l.log.Warn("plugins: writing the log failed", "err", err)
		return
	}
	seen := map[string]bool{}
	var plugins []string
	for _, line := range batch {
		// The host's own log, and the dev log server with it, see each line under its plugin.
		l.log.Log(ctx, slogLevel(line.Level), "plugin."+line.Plugin+": "+line.Text, "method", line.Method, "caller", line.Caller, "call", line.CallID)
		if line.Plugin != "" && !seen[line.Plugin] {
			seen[line.Plugin] = true
			plugins = append(plugins, line.Plugin)
		}
	}
	if l.wrote != nil && len(plugins) > 0 {
		l.wrote(plugins)
	}
}

func slogLevel(level string) slog.Level {
	switch level {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}
