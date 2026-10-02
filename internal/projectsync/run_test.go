package projectsync

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/repogo/host/internal/store"
	"github.com/repogo/host/internal/testwait"
)

// A nudge leads to one pass a settle later, and the rows it moved are told;
// nudges while it waits share that pass.
func TestANudgeTellsTheMovedRows(t *testing.T) {
	db := &recorded{}
	var mu sync.Mutex
	var told []store.ProjectChange
	s := New(listed{"/tmp/p"}, db, "", func(c store.ProjectChange) {
		mu.Lock()
		told = append(told, c)
		mu.Unlock()
	}, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go s.Run(ctx)

	s.Nudge()
	s.Nudge()
	s.Nudge()
	testwait.For(t, "the pass", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(told) > 0
	})
	mu.Lock()
	defer mu.Unlock()
	if len(told) > 2 || len(told[0].Changed) != 1 || told[0].Changed[0].Path != "/tmp/p" {
		t.Fatalf("told %+v", told)
	}
}
