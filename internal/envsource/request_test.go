package envsource

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// peers stands in for every connected phone: it records what the host sent
// and whether a push went out, and can play a phone that is not there.
type peers struct {
	mu      sync.Mutex
	sent    []Request
	alerts  int
	offline bool
	got     chan Request
}

func newPeers() *peers { return &peers{got: make(chan Request, 8)} }

func (p *peers) send(req Request) int {
	p.mu.Lock()
	p.sent = append(p.sent, req)
	offline := p.offline
	p.mu.Unlock()
	p.got <- req
	if offline {
		return 0
	}
	return 1
}

func (p *peers) alert(Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.alerts++
}

func (p *peers) next(t *testing.T) Request {
	t.Helper()
	select {
	case req := <-p.got:
		return req
	case <-time.After(5 * time.Second):
		t.Fatal("no env.request sent")
		return Request{}
	}
}

func newRequests(t *testing.T, p *peers) (*Requests, string) {
	t.Helper()
	s, home := newSources(t)
	return NewRequests(s, "Studio", p.send, p.alert, slog.New(slog.NewTextHandler(io.Discard, nil))), home
}

type resolved struct{ vars map[string]map[string]string }

func resolve(r *Requests, handles ...string) chan resolved {
	out := make(chan resolved, 1)
	go func() {
		out <- resolved{r.Resolve(context.Background(), "/repo", handles, []string{"web"})}
	}()
	return out
}

func wait(t *testing.T, c chan resolved) map[string]map[string]string {
	t.Helper()
	select {
	case got := <-c:
		return got.vars
	case <-time.After(5 * time.Second):
		t.Fatal("the start never resumed")
		return nil
	}
}

func TestHandlesBoundHereNeedNoPhone(t *testing.T) {
	p := newPeers()
	r, home := newRequests(t, p)
	if _, err := r.sources.Set("local", write(t, filepath.Join(home, ".env"), "A=1")); err != nil {
		t.Fatal(err)
	}
	got := wait(t, resolve(r, "local"))
	if got["local"]["A"] != "1" {
		t.Fatalf("vars = %v", got)
	}
	if len(p.sent) != 0 {
		t.Fatalf("sent %d requests for a handle bound here", len(p.sent))
	}
}

func TestApproveFeedsTheWaitingStart(t *testing.T) {
	p := newPeers()
	r, home := newRequests(t, p)
	if _, err := r.sources.Set("local", write(t, filepath.Join(home, ".env"), "A=1")); err != nil {
		t.Fatal(err)
	}
	done := resolve(r, "remote", "local")

	req := p.next(t)
	if req.State != StatePending || req.HostLabel != "Studio" || req.Path != "/repo" ||
		len(req.Handles) != 1 || req.Handles[0] != "remote" || len(req.Services) != 1 {
		t.Fatalf("request = %+v; only the unbound handle should be asked for", req)
	}
	if _, err := time.Parse(time.RFC3339, req.ExpiresAt); err != nil {
		t.Fatalf("expires_at %q: %v", req.ExpiresAt, err)
	}
	if pending := r.Pending(); len(pending) != 1 || pending[0].RequestID != req.RequestID {
		t.Fatalf("pending = %+v", pending)
	}

	err := r.Provide(req.RequestID, true, map[string]ReadResult{
		"remote": {OK: true, Vars: map[string]string{"B": "2"}},
		"extra":  {OK: true, Vars: map[string]string{"C": "3"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := wait(t, done)
	if got["remote"]["B"] != "2" || got["local"]["A"] != "1" || got["extra"] != nil {
		t.Fatalf("vars = %v", got)
	}
	if end := p.next(t); end.RequestID != req.RequestID || end.State != StateDone {
		t.Fatalf("end = %+v, want the same request done", end)
	}
	if len(r.Pending()) != 0 {
		t.Fatal("an answered request is still pending")
	}
	if p.alerts != 0 {
		t.Fatal("pushed although a phone was connected")
	}

	// A late or repeated answer finds nothing.
	if err := r.Provide(req.RequestID, true, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("late provide = %v, want not found", err)
	}
}

func TestDenyStartsWithoutTheVariables(t *testing.T) {
	p := newPeers()
	r, _ := newRequests(t, p)
	done := resolve(r, "remote")
	req := p.next(t)
	if err := r.Provide(req.RequestID, false, map[string]ReadResult{"remote": {OK: true, Vars: map[string]string{"B": "2"}}}); err != nil {
		t.Fatal(err)
	}
	if got := wait(t, done); len(got) != 0 {
		t.Fatalf("vars = %v after a deny", got)
	}
	if end := p.next(t); end.State != StateDone {
		t.Fatalf("end = %+v", end)
	}
}

func TestWrongIDIsNotFound(t *testing.T) {
	p := newPeers()
	r, _ := newRequests(t, p)
	done := resolve(r, "remote")
	req := p.next(t)
	if err := r.Provide("not-"+req.RequestID, true, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong id = %v", err)
	}
	if len(r.Pending()) != 1 {
		t.Fatal("a wrong id resolved the request")
	}
	_ = r.Provide(req.RequestID, false, nil)
	wait(t, done)
}

func TestExpiryStartsWithoutAndEndsTheRequest(t *testing.T) {
	p := newPeers()
	p.offline = true
	r, _ := newRequests(t, p)
	r.timeout = 50 * time.Millisecond
	done := resolve(r, "remote")

	req := p.next(t)
	if got := wait(t, done); len(got) != 0 {
		t.Fatalf("vars = %v after expiry", got)
	}
	if end := p.next(t); end.RequestID != req.RequestID || end.State != StateDone {
		t.Fatalf("end = %+v", end)
	}
	p.mu.Lock()
	alerts := p.alerts
	p.mu.Unlock()
	if alerts != 1 {
		t.Fatalf("alerts = %d, want one push with no phone connected", alerts)
	}
	if err := r.Provide(req.RequestID, true, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("provide after expiry = %v, want not found", err)
	}
}
