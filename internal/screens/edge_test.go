package screens

import (
	"context"
	"sync"
	"testing"

	"github.com/repogoapp/host/internal/testwait"
)

// The phone hears an edge once when it's reached, not again while it holds,
// and again after another input made both sides forget it.
func TestEdgeWatchTellsChanges(t *testing.T) {
	var mu sync.Mutex
	edge := ScrollEdgeTop
	var told []ScrollEdge
	w := &edgeWatch{
		ctx: context.Background(),
		read: func(int, bool) ScrollEdge {
			mu.Lock()
			defer mu.Unlock()
			return edge
		},
		reply: func(v any) error {
			mu.Lock()
			defer mu.Unlock()
			told = append(told, v.(StreamMessage).Edge.Vertical)
			return nil
		},
	}
	scroll := StreamInput{Type: "scroll", Scroll: &ScrollInput{DY: 1}}
	// A scroll reads twice, soon and once it settles: wait for both.
	scrolled := func(in StreamInput) {
		w.mu.Lock()
		before := w.checks
		w.mu.Unlock()
		w.saw(in)
		testwait.For(t, "the scroll's reads", func() bool {
			w.mu.Lock()
			defer w.mu.Unlock()
			return w.checks >= before+2
		})
	}

	scrolled(scroll)
	scrolled(scroll)
	w.saw(StreamInput{Type: "pointer", Pointer: &PointerInput{X: 0.5, Y: 0.5}})
	scrolled(scroll)
	mu.Lock()
	edge = ScrollEdgeNone
	mu.Unlock()
	scrolled(StreamInput{Type: "scroll", Scroll: &ScrollInput{DY: -1}})

	mu.Lock()
	defer mu.Unlock()
	want := []ScrollEdge{ScrollEdgeTop, ScrollEdgeTop, ScrollEdgeNone}
	if len(told) != len(want) {
		t.Fatalf("told %v, want %v", told, want)
	}
	for i := range want {
		if told[i] != want[i] {
			t.Fatalf("told %v, want %v", told, want)
		}
	}
}
