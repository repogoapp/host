package agentusage

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
)

// fixture answers with whatever reading it holds and counts the calls;
// gate, when set, holds each answer until it is closed.
type fixture struct {
	mu    sync.Mutex
	calls int
	next  Usage
	last  Usage
	gate  chan struct{}
	done  chan struct{}
}

func (*fixture) Kind() agent.Kind { return "fixture" }
func (*fixture) Name() string     { return "Fixture" }

func (f *fixture) Usage(_ context.Context, last Usage) Usage {
	f.mu.Lock()
	f.calls++
	f.last = last
	gate, next, done := f.gate, f.next, f.done
	f.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if done != nil {
		defer func() { done <- struct{}{} }()
	}
	return next
}

// A reading is cached for the TTL and stamped with its agent; a failed read
// is handed the last good one to fall back on.
func TestServiceCachesAndKeepsTheLastGoodReading(t *testing.T) {
	f := &fixture{next: Usage{Available: true, CapturedAtMS: 1, Windows: []Window{{ID: "w"}}}}
	s := New(f)
	got := s.One(t.Context(), "fixture")
	if got.Agent != "fixture" || got.Name != "Fixture" || f.calls != 1 {
		t.Fatalf("first = %+v after %d calls", got, f.calls)
	}
	s.One(t.Context(), "fixture")
	if f.calls != 1 {
		t.Fatal("fresh reading not cached")
	}

	delete(s.fresh, "fixture")
	f.next = Usage{}
	if got := s.One(t.Context(), "fixture"); got.Available {
		t.Fatalf("failed read = %+v", got)
	}
	if f.last.CapturedAtMS != 1 || len(f.last.Windows) != 1 {
		t.Fatalf("provider was handed %+v, want the last good reading", f.last)
	}
}

// Past the TTL the old reading answers at once, one read runs behind it, and
// its answer serves the next call.
func TestServiceAnswersStaleAndRefreshesBehind(t *testing.T) {
	f := &fixture{next: Usage{Available: true, CapturedAtMS: 1, Windows: []Window{{ID: "w"}}}}
	s := New(f)
	s.One(t.Context(), "fixture")
	s.fresh["fixture"] = cached{usage: s.fresh["fixture"].usage, at: time.Now().Add(-2 * ttl)}

	gate, done := make(chan struct{}), make(chan struct{}, 1)
	f.mu.Lock()
	f.next = Usage{Available: true, CapturedAtMS: 2, Windows: []Window{{ID: "w"}}}
	f.gate, f.done = gate, done
	f.mu.Unlock()

	if got := s.One(t.Context(), "fixture"); got.CapturedAtMS != 1 {
		t.Fatalf("stale call = %+v, want the old reading at once", got)
	}
	if got := s.One(t.Context(), "fixture"); got.CapturedAtMS != 1 {
		t.Fatalf("second stale call = %+v", got)
	}
	close(gate)
	<-done
	// The read stores its answer just after the provider returns.
	for range 100 {
		if got := s.One(t.Context(), "fixture"); got.CapturedAtMS == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	f.mu.Lock()
	calls := f.calls
	f.mu.Unlock()
	if got := s.One(t.Context(), "fixture"); got.CapturedAtMS != 2 || calls != 2 {
		t.Fatalf("after refresh = %+v, %d calls, want the new reading from one read", got, calls)
	}
}

// A read begun before an invalidation never stores its older answer.
func TestServiceDropsAReadOlderThanAReset(t *testing.T) {
	f := &fixture{next: Usage{Available: true, CapturedAtMS: 1, Windows: []Window{{ID: "w"}}}}
	s := New(f)
	gate, done := make(chan struct{}), make(chan struct{}, 1)
	f.gate, f.done = gate, done
	go s.One(context.Background(), "fixture")
	for {
		f.mu.Lock()
		started := f.calls == 1
		f.mu.Unlock()
		if started {
			break
		}
		time.Sleep(time.Millisecond)
	}
	s.mu.Lock()
	delete(s.fresh, "fixture")
	s.generation["fixture"]++
	s.mu.Unlock()
	close(gate)
	<-done
	time.Sleep(10 * time.Millisecond)
	s.mu.Lock()
	_, stored := s.fresh["fixture"]
	s.mu.Unlock()
	if stored {
		t.Fatal("a read from before the reset was cached")
	}
}
