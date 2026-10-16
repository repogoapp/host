package store

import (
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
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

// A stop with nothing running here settles any active chat, a dead host
// turn or a terminal's; a settled chat is left alone.
func TestInterruptSettlesAnyActiveChat(t *testing.T) {
	db := newStore(t)
	started := time.UnixMilli(1000)
	for _, c := range []struct {
		id     ChatID
		status agent.ChatStatus
		key    string
	}{
		{chatID("claude", "dead"), agent.ChatWorking, ManagerTurnPrefix + "t1"},
		{chatID("claude", "terminal"), agent.ChatWorking, "hook-prompt:p1"},
		{chatID("claude", "done"), agent.ChatIdle, ManagerTurnPrefix + "t3"},
	} {
		if err := db.SetStatus(c.id, "/tmp/a", c.status, started, TurnObservation{Key: c.key, StartedAt: &started}); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[ChatID]bool{
		chatID("claude", "dead"):     true,
		chatID("claude", "terminal"): true,
		chatID("claude", "done"):     false,
	} {
		settled, err := db.Interrupt(id, time.UnixMilli(2000))
		if err != nil {
			t.Fatal(err)
		}
		if settled != want {
			t.Errorf("%s: settled = %v, want %v", id, settled, want)
		}
	}
	chat, err := db.Info(chatID("claude", "terminal"))
	if err != nil {
		t.Fatal(err)
	}
	// Settling is not activity: the turn ends where it was last seen.
	if chat.Status != agent.ChatInterrupted || chat.LastTurnFinishedAt == nil || *chat.LastTurnFinishedAt != 1000 {
		t.Errorf("terminal chat = %s finished %v, want interrupted at 1000", chat.Status, chat.LastTurnFinishedAt)
	}
}

// A Codex CLI killed mid-turn leaves its rollout open for good. Once neither a
// hook nor the file has moved since the cutoff it settles; a quiet host turn
// and a recent one are left, and a line after the settle makes it work again.
func TestInterruptStaleSettlesAbandonedTurns(t *testing.T) {
	db := newStore(t)
	sync := func(id string, updated int64, events ...agent.Event) {
		t.Helper()
		meta := session.Meta{Agent: agent.KindCodex, ID: id, Cwd: "/tmp/a", UpdatedAt: time.UnixMilli(updated), SizeBytes: updated}
		if err := db.SyncBatch([]Entry{{Meta: meta, Events: events}}); err != nil {
			t.Fatal(err)
		}
	}
	open := []agent.Event{
		{Kind: agent.EventTurnStarted, At: 1000},
		{Kind: agent.EventUserMessage, Text: "go", At: 1000},
		{Kind: agent.EventToolCall, At: 1500},
	}
	sync("abandoned", 1500, open...)
	sync("recent", 9500, append(open, agent.Event{Kind: agent.EventToolCall, At: 9500})...)
	started := time.UnixMilli(1000)
	if err := db.SetStatus(chatID("codex", "host"), "/tmp/a", agent.ChatWorking, started,
		TurnObservation{Key: ManagerTurnPrefix + "t1", StartedAt: &started}); err != nil {
		t.Fatal(err)
	}

	n, err := db.InterruptStale(time.UnixMilli(5000), time.UnixMilli(10000))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("settled %d chats, want 1", n)
	}
	status := func(id string) agent.ChatStatus {
		t.Helper()
		chat, err := db.Info(chatID("codex", id))
		if err != nil {
			t.Fatal(err)
		}
		return chat.Status
	}
	// It keeps its place in "Recent activity": its file's last write.
	if chat, _ := db.Info(chatID("codex", "abandoned")); chat.ActivityAt != 1500 {
		t.Errorf("abandoned activity_at = %d, want 1500", chat.ActivityAt)
	}
	for id, want := range map[string]agent.ChatStatus{
		"abandoned": agent.ChatInterrupted,
		"recent":    agent.ChatWorking,
		"host":      agent.ChatWorking,
	} {
		if got := status(id); got != want {
			t.Errorf("%s: status = %s, want %s", id, got, want)
		}
	}

	// A reread of the unchanged file keeps the settle; a new line undoes it.
	sync("abandoned", 1500, open...)
	if got := status("abandoned"); got != agent.ChatInterrupted {
		t.Errorf("after a reread: %s, want interrupted", got)
	}
	sync("abandoned", 11000, append(open, agent.Event{Kind: agent.EventToolCall, At: 11000})...)
	if got := status("abandoned"); got != agent.ChatWorking {
		t.Errorf("after a new line: %s, want working", got)
	}
}
