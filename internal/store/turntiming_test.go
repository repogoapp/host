package store

import (
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
)

func syncEvents(t *testing.T, db *Store, kind agent.Kind, id string, events []agent.Event) Chat {
	t.Helper()
	err := db.Sync(session.Meta{Agent: kind, ID: id, Cwd: "/tmp/" + id, UpdatedAt: time.UnixMilli(9_000)}, events)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	c, err := db.Info(ChatID(string(kind) + ":" + id))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	return c
}

func stamp(t *testing.T, got *int64, want int64) {
	t.Helper()
	if want == 0 {
		if got != nil {
			t.Fatalf("want no stamp, got %d", *got)
		}
		return
	}
	if got == nil || *got != want {
		t.Fatalf("want %d, got %v", want, got)
	}
}

func TestColdIndexTimesTheLastTurnLikeAHookWould(t *testing.T) {
	db := newStore(t)

	// Claude: prompt line to the agent's last record.
	c := syncEvents(t, db, agent.KindClaude, "a", []agent.Event{
		{Kind: agent.EventUserMessage, Text: "one", At: 1_000},
		{Kind: agent.EventText, Text: "a", At: 2_000},
		{Kind: agent.EventUserMessage, Text: "two", At: 5_000},
		{Kind: agent.EventToolCall, Tool: &agent.ToolCall{CallID: "c", Name: "Read"}, At: 6_000},
		{Kind: agent.EventToolResult, Tool: &agent.ToolCall{CallID: "c"}, At: 7_000},
		{Kind: agent.EventText, Text: "b", At: 8_000},
	})
	stamp(t, c.LastTurnStartedAt, 5_000)
	stamp(t, c.LastTurnFinishedAt, 8_000)

	// Codex: lifecycle rows are exact, and a missing finish means still running.
	c = syncEvents(t, db, agent.KindCodex, "b", []agent.Event{
		{Kind: agent.EventUserMessage, Text: "one", At: 1_000},
		{Kind: agent.EventTurnStarted, TurnID: "t1", At: 1_100},
		{Kind: agent.EventText, Text: "a", At: 2_000},
		{Kind: agent.EventTurnFinished, TurnID: "t1", At: 3_000},
		{Kind: agent.EventUserMessage, Text: "two", At: 5_000},
		{Kind: agent.EventTurnStarted, TurnID: "t2", At: 5_100},
		{Kind: agent.EventText, Text: "b", At: 6_000},
	})
	stamp(t, c.LastTurnStartedAt, 5_100)
	stamp(t, c.LastTurnFinishedAt, 0)
}

func TestLiveObserverOwnsTheTurnOverTheTranscript(t *testing.T) {
	db := newStore(t)
	id := ChatID("claude:a")
	events := []agent.Event{
		{Kind: agent.EventUserMessage, Text: "one", At: 5_010},
		{Kind: agent.EventText, Text: "a", At: 6_000},
	}

	// Hook saw the prompt first; the file's slightly later prompt stamp must not take over.
	started := time.UnixMilli(5_000)
	if err := db.SetStatus(id, "/tmp/a", agent.ChatWorking, started, TurnObservation{Key: "hook-prompt:p1", StartedAt: &started}); err != nil {
		t.Fatal(err)
	}
	c := syncEvents(t, db, agent.KindClaude, "a", events)
	stamp(t, c.LastTurnStartedAt, 5_000)
	stamp(t, c.LastTurnFinishedAt, 0)

	// Stop lands with only a finish: same key, so it fills in.
	finished := time.UnixMilli(7_000)
	if err := db.SetStatus(id, "/tmp/a", agent.ChatIdle, finished, TurnObservation{Key: "hook-prompt:p1", FinishedAt: &finished}); err != nil {
		t.Fatal(err)
	}
	c, _ = db.Info(id)
	stamp(t, c.LastTurnStartedAt, 5_000)
	stamp(t, c.LastTurnFinishedAt, 7_000)
}

func TestHookAdoptsATranscriptReadingOfTheSameTurn(t *testing.T) {
	db := newStore(t)
	id := ChatID("claude:a")
	c := syncEvents(t, db, agent.KindClaude, "a", []agent.Event{
		{Kind: agent.EventUserMessage, Text: "one", At: 5_000},
		{Kind: agent.EventText, Text: "a", At: 6_000},
	})
	stamp(t, c.LastTurnStartedAt, 5_000)

	// A Stop with no start keeps the file's start and takes the exact finish.
	finished := time.UnixMilli(7_000)
	if err := db.SetStatus(id, "/tmp/a", agent.ChatIdle, finished, TurnObservation{Key: "hook-prompt:p1", FinishedAt: &finished}); err != nil {
		t.Fatal(err)
	}
	c, _ = db.Info(id)
	stamp(t, c.LastTurnStartedAt, 5_000)
	stamp(t, c.LastTurnFinishedAt, 7_000)

	// The file catching up afterwards does not displace the hook.
	c = syncEvents(t, db, agent.KindClaude, "a", []agent.Event{
		{Kind: agent.EventUserMessage, Text: "one", At: 5_000},
		{Kind: agent.EventText, Text: "a", At: 6_000},
		{Kind: agent.EventText, Text: "b", At: 6_500},
	})
	stamp(t, c.LastTurnStartedAt, 5_000)
	stamp(t, c.LastTurnFinishedAt, 7_000)
}

// A Codex turn run outside this host has no hook: once the host's own turn has
// ended, the file's later turn takes the row, and the sync reports it moved.
func TestTranscriptTurnAfterALiveOneTakesTheRow(t *testing.T) {
	db := newStore(t)
	id := ChatID("codex:a")
	started, finished := time.UnixMilli(1_000), time.UnixMilli(3_000)
	if err := db.SetStatus(id, "/tmp/a", agent.ChatWorking, started, TurnObservation{Key: ManagerTurnPrefix + "t1", StartedAt: &started}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetStatus(id, "/tmp/a", agent.ChatIdle, finished, TurnObservation{Key: ManagerTurnPrefix + "t1", FinishedAt: &finished}); err != nil {
		t.Fatal(err)
	}
	meta := session.Meta{Agent: agent.KindCodex, ID: "a", Cwd: "/tmp/a", UpdatedAt: time.UnixMilli(9_000)}
	ownTurn := []agent.Event{
		{Kind: agent.EventUserMessage, Text: "one", At: 1_000},
		{Kind: agent.EventTurnStarted, TurnID: "t1", At: 1_100},
		{Kind: agent.EventTurnFinished, TurnID: "t1", At: 2_900},
	}

	// The file's reading of the host's own turn changes nothing.
	heard := listen(db)
	if err := db.SyncBatch([]Entry{{Meta: meta, Events: ownTurn}}); err != nil {
		t.Fatal(err)
	}
	c, _ := db.Info(id)
	stamp(t, c.LastTurnStartedAt, 1_000)
	stamp(t, c.LastTurnFinishedAt, 3_000)
	// The first transcript behind the placeholder is heard; nothing else moved.
	if got := heard(); len(got) != 1 || len(got[0].Changed) != 1 {
		t.Fatalf("own turn: heard %v, want the first transcript only", got)
	}

	outside := append(ownTurn,
		agent.Event{Kind: agent.EventUserMessage, Text: "two", At: 5_000},
		agent.Event{Kind: agent.EventTurnStarted, TurnID: "t2", At: 5_100},
	)
	heard = listen(db)
	if err := db.SyncBatch([]Entry{{Meta: meta, Events: outside}}); err != nil {
		t.Fatal(err)
	}
	c, _ = db.Info(id)
	stamp(t, c.LastTurnStartedAt, 5_100)
	stamp(t, c.LastTurnFinishedAt, 0)
	if got := heard(); c.Status != agent.ChatWorking || len(got) != 1 || len(got[0].Changed) != 1 || got[0].Changed[0] != id {
		t.Fatalf("outside turn: status %s, heard %v", c.Status, got)
	}
}

// Claude fires its prompt hook seconds after the host starts the turn; the
// list must keep the host's stamp, the one the open chat counts from.
func TestPromptHookDoesNotRestampAHostTurn(t *testing.T) {
	db := newStore(t)
	id := ChatID("claude:a")
	started, hooked, finished := time.UnixMilli(1_000), time.UnixMilli(12_000), time.UnixMilli(20_000)
	if err := db.SetStatus(id, "/tmp/a", agent.ChatWorking, started, TurnObservation{Key: ManagerTurnPrefix + "t1", StartedAt: &started}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetStatus(id, "/tmp/a", agent.ChatWorking, hooked, TurnObservation{Key: "hook-prompt:p1", StartedAt: &hooked}); err != nil {
		t.Fatal(err)
	}
	c, _ := db.Info(id)
	stamp(t, c.LastTurnStartedAt, 1_000)
	stamp(t, c.LastTurnFinishedAt, 0)

	if err := db.SetStatus(id, "/tmp/a", agent.ChatIdle, finished, TurnObservation{Key: ManagerTurnPrefix + "t1", FinishedAt: &finished}); err != nil {
		t.Fatal(err)
	}
	c, _ = db.Info(id)
	stamp(t, c.LastTurnStartedAt, 1_000)
	stamp(t, c.LastTurnFinishedAt, 20_000)

	// Once the host's turn has ended, a terminal's next prompt takes the row.
	next := time.UnixMilli(30_000)
	if err := db.SetStatus(id, "/tmp/a", agent.ChatWorking, next, TurnObservation{Key: "hook-prompt:p2", StartedAt: &next}); err != nil {
		t.Fatal(err)
	}
	c, _ = db.Info(id)
	stamp(t, c.LastTurnStartedAt, 30_000)
	stamp(t, c.LastTurnFinishedAt, 0)
}
