package agentusage

import (
	"context"
	"testing"

	"github.com/repogo/host/internal/agent"
)

// fixture answers with whatever reading it holds and counts the calls.
type fixture struct {
	calls int
	next  Usage
	last  Usage
}

func (*fixture) Kind() agent.Kind { return "fixture" }
func (*fixture) Name() string     { return "Fixture" }

func (f *fixture) Usage(_ context.Context, last Usage) Usage {
	f.calls++
	f.last = last
	return f.next
}

// A reading is cached for the TTL and stamped with its agent; a failed read
// is handed the last good one to fall back on.
func TestServiceCachesAndKeepsTheLastGoodReading(t *testing.T) {
	f := &fixture{next: Usage{Available: true, CapturedAtMS: 1, Windows: []Window{{ID: "w"}}}}
	s := New([]agent.Kind{"fixture"}, f)
	got := s.One(t.Context(), "fixture")
	if got.Agent != "fixture" || got.Name != "Fixture" || f.calls != 1 {
		t.Fatalf("first = %+v after %d calls", got, f.calls)
	}
	s.One(t.Context(), "fixture")
	if f.calls != 1 {
		t.Fatal("fresh reading not cached")
	}

	s.fresh = map[agent.Kind]cached{}
	f.next = Usage{Detail: "down"}
	if got := s.One(t.Context(), "fixture"); got.Available {
		t.Fatalf("failed read = %+v", got)
	}
	if f.last.CapturedAtMS != 1 || len(f.last.Windows) != 1 {
		t.Fatalf("provider was handed %+v, want the last good reading", f.last)
	}
}
