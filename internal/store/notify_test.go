package store

import (
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
)

// listen records every change the store reports and returns a function that
// hands them over, emptying the record.
func listen(db *Store) func() []Change {
	var heard []Change
	db.Notify(func(c Change) { heard = append(heard, c) })
	return func() []Change {
		out := heard
		heard = nil
		return out
	}
}

func one(t *testing.T, heard func() []Change, want Change) {
	t.Helper()
	got := heard()
	if len(got) != 1 {
		t.Fatalf("heard %v, want one change %v", got, want)
	}
	if len(got[0].Changed) != len(want.Changed) || len(got[0].Removed) != len(want.Removed) {
		t.Fatalf("heard %v, want %v", got[0], want)
	}
	for i := range want.Changed {
		if got[0].Changed[i] != want.Changed[i] {
			t.Fatalf("heard %v, want %v", got[0], want)
		}
	}
	for i := range want.Removed {
		if got[0].Removed[i] != want.Removed[i] {
			t.Fatalf("heard %v, want %v", got[0], want)
		}
	}
}

// Every write that moves the chat list tells the listener once, after commit,
// so no caller has to remember to publish.
func TestWritesTellTheListener(t *testing.T) {
	db := newStore(t)
	heard := listen(db)
	id := ChatID("claude:a")
	now := time.UnixMilli(5_000)

	// A hook ahead of its file lists a placeholder; the file then fills it.
	if err := db.SetStatus(id, "/tmp/a", agent.ChatWorking, now, TurnObservation{}); err != nil {
		t.Fatal(err)
	}
	one(t, heard, Change{Changed: []ChatID{id}})
	if err := db.SetStatus(id, "/tmp/a", agent.ChatWorking, now, TurnObservation{}); err != nil {
		t.Fatal(err)
	}
	one(t, heard, Change{Changed: []ChatID{id}})
	write(t, db, "a", 5_000, 2)
	one(t, heard, Change{Changed: []ChatID{id}})

	// A resync that moves nothing is silent.
	write(t, db, "a", 5_000, 2)
	if got := heard(); len(got) != 0 {
		t.Fatalf("an unchanged resync was heard: %v", got)
	}

	// A chat seen for the first time is heard, as is a resolve either way.
	write(t, db, "b", 6_000, 1)
	one(t, heard, Change{Changed: []ChatID{"claude:b"}})
	if err := db.SetResolved(id, true, now); err != nil {
		t.Fatal(err)
	}
	one(t, heard, Change{Changed: []ChatID{id}})
	if cleared, err := db.UnresolveIfActive(id, now.Add(time.Second)); err != nil || !cleared {
		t.Fatalf("unresolve: cleared %v, err %v", cleared, err)
	}
	one(t, heard, Change{Changed: []ChatID{id}})
	if _, err := db.UnresolveIfActive(id, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := heard(); len(got) != 0 {
		t.Fatalf("a no-op unresolve was heard: %v", got)
	}

	// Removals, by the user and by a sweep that no longer finds the file.
	if err := db.Delete(id); err != nil {
		t.Fatal(err)
	}
	one(t, heard, Change{Removed: []ChatID{id}})
	if n, err := db.Prune(map[string]bool{}); err != nil || n != 1 {
		t.Fatalf("prune: %d, %v", n, err)
	}
	one(t, heard, Change{Removed: []ChatID{"claude:b"}})
}

// A turn this host ran and left active at shutdown is settled at the next
// start, and the listener hears which chats that touched.
func TestInterruptOrphansTellsTheListener(t *testing.T) {
	db := newStore(t)
	id := ChatID("codex:orphan")
	started := time.UnixMilli(1_000)
	if err := db.SetStatus(id, "/tmp/o", agent.ChatWorking, started, TurnObservation{Key: ManagerTurnPrefix + "t1", StartedAt: &started}); err != nil {
		t.Fatal(err)
	}
	heard := listen(db)
	if n, err := db.InterruptOrphans(started.Add(time.Minute)); err != nil || n != 1 {
		t.Fatalf("orphans: %d, %v", n, err)
	}
	one(t, heard, Change{Changed: []ChatID{id}})
	if c, err := db.Info(id); err != nil || c.Status != agent.ChatInterrupted {
		t.Fatalf("status after settling: %v, %v", c.Status, err)
	}
}

// The placeholder a live turn leaves is heard again once its transcript is
// read, since only then does the row carry a title and a reply.
func TestFirstTranscriptBehindAPlaceholderIsHeard(t *testing.T) {
	db := newStore(t)
	id := ChatID("codex:p")
	at := time.UnixMilli(1_000)
	if err := db.SetStatus(id, "/tmp/p", agent.ChatIdle, at, TurnObservation{}); err != nil {
		t.Fatal(err)
	}
	heard := listen(db)
	meta := session.Meta{Agent: agent.KindCodex, ID: "p", Cwd: "/tmp/p", UpdatedAt: at, SizeBytes: 1}
	if err := db.Sync(meta, []agent.Event{{Kind: agent.EventUserMessage, Text: "hello"}}); err != nil {
		t.Fatal(err)
	}
	one(t, heard, Change{Changed: []ChatID{id}})
	if c, _ := db.Info(id); c.Title == "" {
		t.Fatal("the transcript did not fill the placeholder's title")
	}
}
