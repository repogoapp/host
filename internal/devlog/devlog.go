// Package devlog copies the host's log records to the dev log server
// (apps/log-server) when REPOGO_LOG_SERVER names it, so the host's and the
// phone's logs land in one queryable place. A release host never sets it.
package devlog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"time"
)

// EnvVar names the log server's base URL; scripts/restart.sh sets it.
const EnvVar = "REPOGO_LOG_SERVER"

const (
	queueSize  = 4096
	batchSize  = 256
	flushEvery = 500 * time.Millisecond
	// A dev Mac's own server answers at once; a slow one costs dropped
	// records, never a waiting host.
	postTimeout = 2 * time.Second
)

// Tee is next, with every record at debug and above also queued for the log
// server at base. Handling never waits on the network: a full queue drops
// the record, and the drop is reported in the next batch.
func Tee(ctx context.Context, next slog.Handler, base string) slog.Handler {
	s := newSender(strings.TrimSuffix(base, "/")+"/api/dev/logs", queueSize)
	go s.run(ctx)
	return &handler{next: next, sender: s}
}

// entry is one record in the shape the log server takes (IncomingEntry).
type entry struct {
	Source    string         `json:"source"`
	Level     string         `json:"level"`
	Category  string         `json:"category"`
	Message   string         `json:"message"`
	Timestamp string         `json:"timestamp"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

type handler struct {
	next   slog.Handler
	sender *sender
	// From WithAttrs, keyed with the groups open when they were added.
	attrs []slog.Attr
	group string
}

func (h *handler) Enabled(context.Context, slog.Level) bool { return true }

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	h.sender.enqueue(h.entry(r))
	if !h.next.Enabled(ctx, r.Level) {
		return nil
	}
	return h.next.Handle(ctx, r)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.next = h.next.WithAttrs(attrs)
	c.attrs = slices.Clip(h.attrs)
	for _, a := range attrs {
		c.attrs = append(c.attrs, slog.Attr{Key: h.group + a.Key, Value: a.Value})
	}
	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	c := *h
	c.next = h.next.WithGroup(name)
	c.group = h.group + name + "."
	return &c
}

// entry splits a "chatsync: sweep completed" message into its category and
// text, as the host's packages write them.
func (h *handler) entry(r slog.Record) entry {
	category, message, ok := strings.Cut(r.Message, ": ")
	if !ok || strings.ContainsAny(category, " \t") {
		category, message = "host", r.Message
	}
	metadata := make(map[string]any, len(h.attrs)+r.NumAttrs())
	for _, a := range h.attrs {
		metadata[a.Key] = value(a.Value)
	}
	r.Attrs(func(a slog.Attr) bool {
		metadata[h.group+a.Key] = value(a.Value)
		return true
	})
	return entry{
		Source: "host", Level: level(r.Level), Category: category, Message: message,
		Timestamp: r.Time.UTC().Format(time.RFC3339Nano), Metadata: metadata,
	}
}

// value is an attribute as JSON can carry it: numbers and booleans as they
// are, everything else (errors, durations, structs) as its text.
func value(v slog.Value) any {
	v = v.Resolve()
	switch v.Kind() {
	case slog.KindInt64, slog.KindUint64, slog.KindFloat64, slog.KindBool:
		return v.Any()
	default:
		return v.String()
	}
}

// level is the log server's vocabulary, which the phone's entries use too.
func level(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "error"
	case l >= slog.LevelWarn:
		return "warning"
	case l >= slog.LevelInfo:
		return "info"
	default:
		return "debug"
	}
}

// sender batches queued entries to the log server on its own goroutine.
type sender struct {
	url     string
	queue   chan entry
	client  *http.Client
	dropped atomic.Int64
}

func newSender(url string, size int) *sender {
	return &sender{url: url, queue: make(chan entry, size), client: &http.Client{Timeout: postTimeout}}
}

func (s *sender) enqueue(e entry) {
	select {
	case s.queue <- e:
	default:
		s.dropped.Add(1)
	}
}

func (s *sender) run(ctx context.Context) {
	ticker := time.NewTicker(flushEvery)
	defer ticker.Stop()
	var batch []entry
	for {
		select {
		case <-ctx.Done():
			// The host's last words, such as "shutting down", still go.
			s.post(context.WithoutCancel(ctx), batch)
			return
		case e := <-s.queue:
			if batch = append(batch, e); len(batch) >= batchSize {
				s.post(ctx, batch)
				batch = nil
			}
		case <-ticker.C:
			s.post(ctx, batch)
			batch = nil
		}
	}
}

// post sends a batch, with a note of any records the full queue dropped. A
// failed send is dropped too: these are dev logs, and the file log has them.
func (s *sender) post(ctx context.Context, batch []entry) {
	if n := s.dropped.Swap(0); n > 0 {
		batch = append(batch, entry{Source: "host", Level: "warning", Category: "devlog",
			Message: fmt.Sprintf("dropped %d records: the queue was full", n), Timestamp: time.Now().UTC().Format(time.RFC3339Nano)})
	}
	if len(batch) == 0 {
		return
	}
	body, err := json.Marshal(batch)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if res, err := s.client.Do(req); err == nil {
		res.Body.Close()
	}
}
