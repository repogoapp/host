package remotehost

import (
	"context"
	"testing"
	"time"
)

// A look is reused for two minutes, then runs again; fresh always runs, and
// nothing runs before someone asks.
func TestListLooksOnlyWhenAskedAndReusesARecentLook(t *testing.T) {
	f := &fake{config: "Host studio\n", answers: map[string]answer{
		"studio": {stdout: `{"host_id": "H1", "name": "Studio"}`},
	}}
	s := NewService(f.deps(""))
	now := time.UnixMilli(1_000_000)
	s.now = func() time.Time { return now }
	if len(f.ran) != 0 {
		t.Fatal("a look ran before anyone asked")
	}

	first := s.List(context.Background(), false)
	if len(first.Hosts) != 1 || first.Hosts[0].State != Ready || len(f.ran) != 1 {
		t.Fatalf("first List = %+v after %d SSH calls, want studio ready after 1", first, len(f.ran))
	}
	f.answers["studio"] = answer{stderr: "ssh: connect to host studio: timed out", code: 255}

	now = now.Add(Reuse - time.Second)
	if got := s.List(context.Background(), false); got.Hosts[0].State != Ready || len(f.ran) != 1 {
		t.Fatalf("List under Reuse = %+v after %d SSH calls, want the kept look", got, len(f.ran))
	}
	if got := s.List(context.Background(), true); got.Hosts[0].State != Offline || len(f.ran) != 2 {
		t.Fatalf("fresh List = %+v after %d SSH calls, want a new look", got, len(f.ran))
	}
	now = now.Add(Reuse)
	if s.List(context.Background(), false); len(f.ran) != 3 {
		t.Fatalf("List past Reuse made %d SSH calls, want a third look", len(f.ran))
	}
}

// A look cut short keeps the last full one rather than calling every host offline.
func TestALookCutShortKeepsTheLastFullOne(t *testing.T) {
	f := &fake{config: "Host studio\n", answers: map[string]answer{
		"studio": {stdout: `{"host_id": "H1", "name": "Studio"}`},
	}}
	s := NewService(f.deps(""))
	s.List(context.Background(), false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.List(ctx, true); len(got.Hosts) != 1 || got.Hosts[0].State != Ready {
		t.Fatalf("cancelled List = %+v, want the last full look", got)
	}
}
