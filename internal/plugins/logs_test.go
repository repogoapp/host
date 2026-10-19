package plugins

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/testwait"
)

// openLogs is a log in a temporary folder with the limits a test asks for, and
// the batches its writer reported, read under the lock the writer takes.
func openLogs(t *testing.T, limits LogLimits) (*Logs, func() [][]string) {
	t.Helper()
	var mu sync.Mutex
	var wrote [][]string
	l, err := OpenLogs(t.Context(), filepath.Join(t.TempDir(), "logs.db"), limits, slog.New(slog.NewTextHandler(io.Discard, nil)), func(plugins []string) {
		mu.Lock()
		wrote = append(wrote, plugins)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	return l, func() [][]string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(wrote)
	}
}

// written waits for the writer to land `want` lines and answers the newest thousand.
func written(t *testing.T, l *Logs, want int) []LogLine {
	t.Helper()
	testwait.For(t, fmt.Sprintf("%d lines written", want), func() bool {
		var n int
		return l.db.QueryRow(`SELECT COUNT(*) FROM logs`).Scan(&n) == nil && n >= want
	})
	lines, err := l.Query(t.Context(), LogQuery{Limit: logsLimitMax})
	if err != nil {
		t.Fatal(err)
	}
	return lines
}

// addAll queues records a queue's worth at a time, waiting for each to land, so none is dropped.
func addAll(t *testing.T, l *Logs, dir string, records []logRecord) {
	t.Helper()
	for i := 0; i < len(records); i += logQueueMax {
		l.Add(dir, records[i:min(i+logQueueMax, len(records))])
		written(t, l, min(i+logQueueMax, len(records)))
	}
}

func TestLinesAreKeptWithTheirCallAndQueried(t *testing.T) {
	l, wrote := openLogs(t, defaultLogLimits)
	l.Add("/p/.repogo/plugins", []logRecord{
		{At: 1000, Plugin: "notes", Level: "info", Method: "notes.add", Caller: "agent", CallID: "c1", ChatID: "A-7", Text: "adding", Data: json.RawMessage(`["adding",{"text":"hi"}]`)},
		{At: 2000, Plugin: "notes", Level: "error", Method: "notes.add", Caller: "agent", CallID: "c1", ChatID: "A-7", Text: "it broke"},
		{At: 3000, Plugin: "digest", Level: "debug", Method: "digest.sum", Caller: "card", CallID: "c2", Text: "summing"},
	})
	lines := written(t, l, 3)
	if lines[0].Text != "summing" || lines[2].Text != "adding" {
		t.Fatalf("newest first, got %+v", lines)
	}
	if got := lines[2]; got.Dir != "/p/.repogo/plugins" || got.CallID != "c1" || got.ChatID != "A-7" || string(got.Data) != `["adding",{"text":"hi"}]` {
		t.Fatalf("line = %+v", got)
	}
	if string(lines[1].Data) != "[]" {
		t.Fatalf("a line without data carries an empty list, got %s", lines[1].Data)
	}

	cases := map[string]struct {
		q    LogQuery
		want []string
	}{
		"by plugin":    {LogQuery{Plugin: "notes"}, []string{"it broke", "adding"}},
		"warn and up":  {LogQuery{Level: "warn"}, []string{"it broke"}},
		"debug and up": {LogQuery{Level: "debug"}, []string{"summing", "it broke", "adding"}},
		"by method":    {LogQuery{Method: "digest.sum"}, []string{"summing"}},
		"since":        {LogQuery{Since: 2000}, []string{"summing", "it broke"}},
		"until":        {LogQuery{Until: 2000}, []string{"adding"}},
		"text":         {LogQuery{Query: "brok"}, []string{"it broke"}},
		"limit":        {LogQuery{Limit: 1}, []string{"summing"}},
	}
	for name, c := range cases {
		got, err := l.Query(t.Context(), c.q)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var texts []string
		for _, line := range got {
			texts = append(texts, line.Text)
		}
		if strings.Join(texts, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: got %v, want %v", name, texts, c.want)
		}
	}
	if _, err := l.Query(t.Context(), LogQuery{Level: "loud"}); err == nil {
		t.Fatal("an unknown level is refused")
	}
	errs, err := l.Errors(t.Context(), 0)
	if err != nil || errs[errorsKey("/p/.repogo/plugins", "notes")] != 1 || len(errs) != 1 {
		t.Fatalf("errors = %v %v", errs, err)
	}
	testwait.For(t, "the batch reported", func() bool { return len(wrote()) > 0 })
	if got := wrote(); strings.Join(got[0], ",") != "notes,digest" {
		t.Fatalf("wrote = %v, want the batch's plugins once each", got)
	}
}

func TestPurgeDropsOldLinesThenTheNoisiestPluginsOldestHour(t *testing.T) {
	limits := LogLimits{Keep: 7 * 24 * time.Hour, MaxBytes: 1 << 62, Every: time.Hour}
	l, _ := openLogs(t, limits)
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	hour := int64(time.Hour / time.Millisecond)
	var records []logRecord
	// quiet: one line an hour for the last day. noisy: two hundred an hour for two days, and one too old to keep.
	for h := int64(0); h < 24; h++ {
		records = append(records, logRecord{At: now.UnixMilli() - h*hour, Plugin: "quiet", Level: "info", Text: "tick"})
	}
	for h := int64(0); h < 48; h++ {
		for i := 0; i < 200; i++ {
			records = append(records, logRecord{At: now.UnixMilli() - h*hour, Plugin: "noisy", Level: "info", Text: strings.Repeat("x", 200)})
		}
	}
	records = append(records, logRecord{At: now.Add(-8 * 24 * time.Hour).UnixMilli(), Plugin: "noisy", Level: "info", Text: "ancient"})
	addAll(t, l, "/d", records)

	if err := l.Purge(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if got, _ := l.Query(t.Context(), LogQuery{Query: "ancient"}); len(got) != 0 {
		t.Fatal("a line past Keep stays")
	}
	used, err := l.usedBytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	// Under half of what the file holds, so more than one of noisy's hours must go.
	l.limits.MaxBytes = used / 2
	if err := l.Purge(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if after, _ := l.usedBytes(t.Context()); after > l.limits.MaxBytes {
		t.Fatalf("file is %d bytes, over %d", after, l.limits.MaxBytes)
	}
	quiet, _ := l.Query(t.Context(), LogQuery{Plugin: "quiet", Limit: logsLimitMax})
	if len(quiet) != 24 {
		t.Fatalf("quiet lost lines: %d left of 24", len(quiet))
	}
	var oldest, count int64
	if err := l.db.QueryRow(`SELECT MIN(at), COUNT(*) FROM logs WHERE plugin = 'noisy'`).Scan(&oldest, &count); err != nil {
		t.Fatal(err)
	}
	if oldest <= now.UnixMilli()-47*hour {
		t.Fatalf("noisy's oldest line is still at %d: its oldest hours were not the ones dropped", oldest)
	}
	if count%200 != 0 || count == 0 {
		t.Fatalf("noisy keeps %d lines: whole hours go, oldest first", count)
	}
}

func TestAFullQueueDropsTheOldestAndSaysSo(t *testing.T) {
	l, _ := openLogs(t, defaultLogLimits)
	// Queued behind the writer's back, so the Add below is one batch past the cap.
	l.mu.Lock()
	for i := 0; i < logQueueMax+10; i++ {
		l.queue = append(l.queue, LogLine{At: int64(i), Plugin: "p", Level: "info", Text: fmt.Sprint(i), Data: json.RawMessage("[]")})
	}
	l.mu.Unlock()
	l.Add("/d", []logRecord{{At: 99999, Plugin: "p", Level: "info", Text: "last"}})
	var warn []LogLine
	testwait.For(t, "the batch written", func() bool {
		warn, _ = l.Query(t.Context(), LogQuery{Level: "warn"})
		return len(warn) == 1
	})
	if warn[0].Text != "dropped 11 lines: the log queue was full" {
		t.Fatalf("warn = %+v", warn[0])
	}
	if first, _ := l.Query(t.Context(), LogQuery{Until: 11}); len(first) != 0 {
		t.Fatalf("the oldest lines were kept: %+v", first)
	}
	if last, _ := l.Query(t.Context(), LogQuery{Query: "last"}); len(last) != 1 {
		t.Fatal("the newest line was dropped")
	}
}
