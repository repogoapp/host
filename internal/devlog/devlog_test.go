package devlog

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/repogo/host/internal/testwait"
)

// A record reaches the log server split into category and message, with its
// attributes (groups included) as metadata, at debug even when the file log
// is at info.
func TestRecordsReachTheLogServer(t *testing.T) {
	var mu sync.Mutex
	var got []entry
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/dev/logs" {
			t.Errorf("posted to %s", r.URL.Path)
		}
		var batch []entry
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
		}
		mu.Lock()
		got = append(got, batch...)
		mu.Unlock()
	}))
	defer server.Close()

	file := slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})
	log := slog.New(Tee(t.Context(), file, server.URL+"/")).With("device", "phone")
	log.WithGroup("turn").Info("chatsync: sweep completed", "sessions", 3, "err", errors.New("boom"))
	log.Debug("turn started")

	testwait.For(t, "both records", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 2
	})
	sweep, start := got[0], got[1]
	if sweep.Source != "host" || sweep.Category != "chatsync" || sweep.Message != "sweep completed" || sweep.Level != "info" {
		t.Errorf("sweep = %+v", sweep)
	}
	if sweep.Metadata["device"] != "phone" || sweep.Metadata["turn.sessions"] != float64(3) || sweep.Metadata["turn.err"] != "boom" {
		t.Errorf("sweep metadata = %v", sweep.Metadata)
	}
	if start.Category != "host" || start.Message != "turn started" || start.Level != "debug" {
		t.Errorf("debug record = %+v", start)
	}
}

// Logging never waits on the server: past the queue, records are dropped and
// counted, and the count goes out with the next batch.
func TestAFullQueueDropsInsteadOfBlocking(t *testing.T) {
	s := newSender("http://127.0.0.1:1/api/dev/logs", 1)
	s.enqueue(entry{Message: "kept"})
	s.enqueue(entry{Message: "dropped"})
	s.enqueue(entry{Message: "dropped"})
	if n := s.dropped.Load(); n != 2 {
		t.Fatalf("dropped %d, want 2", n)
	}
}
