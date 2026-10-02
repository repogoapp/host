package store

import (
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
)

// Only this host's own turns die with it; a CLI in a terminal may still be
// working, so its status is left for its hooks to settle.
func TestInterruptOrphansSettlesOnlyThisHostsTurns(t *testing.T) {
	db := newStore(t)
	started := time.UnixMilli(1000)
	for _, c := range []struct {
		id     ChatID
		status agent.ChatStatus
		key    string
	}{
		{chatID("codex", "orphan"), agent.ChatWorking, ManagerTurnPrefix + "t1"},
		{chatID("codex", "asking"), agent.ChatAwaitingUser, ManagerTurnPrefix + "t2"},
		{chatID("codex", "done"), agent.ChatIdle, ManagerTurnPrefix + "t3"},
		{chatID("claude", "terminal"), agent.ChatWorking, "hook-prompt:p1"},
	} {
		if err := db.SetStatus(c.id, "/tmp/a", c.status, started, TurnObservation{Key: c.key, StartedAt: &started}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := db.InterruptOrphans(time.UnixMilli(2000))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("settled %d chats, want 2", n)
	}
	for id, want := range map[ChatID]agent.ChatStatus{
		chatID("codex", "orphan"):    agent.ChatInterrupted,
		chatID("codex", "asking"):    agent.ChatInterrupted,
		chatID("codex", "done"):      agent.ChatIdle,
		chatID("claude", "terminal"): agent.ChatWorking,
	} {
		chat, err := db.Info(id)
		if err != nil {
			t.Fatal(err)
		}
		if chat.Status != want {
			t.Errorf("%s: status = %s, want %s", id, chat.Status, want)
		}
	}
}
