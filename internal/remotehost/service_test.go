package remotehost

import (
	"context"
	"testing"
	"time"
)

func TestLastWaitsForTheFirstRunThenKeepsIt(t *testing.T) {
	f := &fake{config: "Host studio\n", answers: map[string]answer{
		"studio": {stdout: `{"host_id": "H1", "name": "Studio"}`},
	}}
	s := NewService(f.deps(""))

	short, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if got := s.Last(short); len(got.Hosts) != 0 || got.CheckedAt != 0 {
		t.Fatalf("Last before any run = %+v, want empty", got)
	}

	ran := s.Refresh(context.Background())
	if len(ran.Hosts) != 1 || ran.Hosts[0].HostID != "H1" || ran.CheckedAt == 0 {
		t.Fatalf("Refresh = %+v, want studio as H1", ran)
	}
	f.answers["studio"] = answer{stderr: "ssh: connect to host studio: timed out", code: 255}
	if got := s.Last(context.Background()); got.Hosts[0].State != Ready {
		t.Fatalf("Last = %+v, want the kept run, not a new look", got)
	}
}

func TestARefreshCutShortKeepsTheLastFullRun(t *testing.T) {
	f := &fake{config: "Host studio\n", answers: map[string]answer{
		"studio": {stdout: `{"host_id": "H1", "name": "Studio"}`},
	}}
	s := NewService(f.deps(""))
	s.Refresh(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.Refresh(ctx); len(got.Hosts) != 1 || got.Hosts[0].State != Ready {
		t.Fatalf("cancelled Refresh = %+v, want the last full run", got)
	}
}
