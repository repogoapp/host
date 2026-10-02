package store

import (
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
)

func order(t *testing.T, db *Store) []ChatID {
	t.Helper()
	page, err := db.Chats(ChatQuery{})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]ChatID, len(page.Chats))
	for i, c := range page.Chats {
		ids[i] = c.ID
	}
	return ids
}

func TestAStreamingTurnDoesNotMoveItsChatUntilItEnds(t *testing.T) {
	db := newStore(t)
	sync := func(id string, mtime int64, events []agent.Event) {
		t.Helper()
		err := db.Sync(session.Meta{Agent: agent.KindCodex, ID: id, Cwd: "/tmp/" + id,
			UpdatedAt: time.UnixMilli(mtime), SizeBytes: int64(len(events))}, events)
		if err != nil {
			t.Fatal(err)
		}
	}
	older := []agent.Event{
		{Kind: agent.EventUserMessage, Text: "one", At: 1_000},
		{Kind: agent.EventTurnStarted, TurnID: "t1", At: 1_000},
	}
	newer := []agent.Event{
		{Kind: agent.EventUserMessage, Text: "one", At: 2_000},
		{Kind: agent.EventTurnStarted, TurnID: "t1", At: 2_000},
	}
	sync("a", 1_000, older)
	sync("b", 2_000, newer)

	// "a" keeps writing tool calls; its file is now the newest on disk.
	older = append(older,
		agent.Event{Kind: agent.EventToolCall, Tool: &agent.ToolCall{CallID: "c", Name: "Read"}, At: 3_000},
		agent.Event{Kind: agent.EventToolResult, Tool: &agent.ToolCall{CallID: "c"}, At: 4_000})
	sync("a", 4_000, older)
	if got := order(t, db); got[0] != "codex:b" {
		t.Fatalf("a streaming chat moved up: %v", got)
	}

	// Its turn ending does move it.
	older = append(older, agent.Event{Kind: agent.EventTurnFinished, TurnID: "t1", At: 5_000})
	sync("a", 5_000, older)
	if got := order(t, db); got[0] != "codex:a" {
		t.Fatalf("a finished turn did not move its chat up: %v", got)
	}
	c, err := db.Info("codex:a")
	if err != nil {
		t.Fatal(err)
	}
	if c.ActivityAt != 5_000 {
		t.Fatalf("activity_at = %d, want the finish", c.ActivityAt)
	}
}

func TestAClaudeTranscriptFinishIsNotActivity(t *testing.T) {
	db := newStore(t)
	// The file's "finish" is only its newest line; the prompt is the activity.
	c := syncEvents(t, db, agent.KindClaude, "a", []agent.Event{
		{Kind: agent.EventUserMessage, Text: "one", At: 5_000},
		{Kind: agent.EventText, Text: "a", At: 8_000},
	})
	if c.ActivityAt != 5_000 {
		t.Fatalf("activity_at = %d, want the prompt", c.ActivityAt)
	}

	// A hook's Stop is the turn actually ending.
	id := ChatID("claude:b")
	started, finished := time.UnixMilli(5_000), time.UnixMilli(9_000)
	if err := db.SetStatus(id, "/tmp/b", agent.ChatWorking, started, TurnObservation{Key: "hook-prompt:p", StartedAt: &started}); err != nil {
		t.Fatal(err)
	}
	if c, _ = db.Info(id); c.ActivityAt != 5_000 {
		t.Fatalf("running: activity_at = %d, want the prompt", c.ActivityAt)
	}
	if err := db.SetStatus(id, "/tmp/b", agent.ChatIdle, finished, TurnObservation{Key: "hook-prompt:p", FinishedAt: &finished}); err != nil {
		t.Fatal(err)
	}
	if c, _ = db.Info(id); c.ActivityAt != 9_000 {
		t.Fatalf("stopped: activity_at = %d, want the finish", c.ActivityAt)
	}
}

func TestAChatWithNoTurnFallsBackToItsFile(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 700, 1)
	c, err := db.Info("claude:a")
	if err != nil {
		t.Fatal(err)
	}
	if c.ActivityAt != 700 {
		t.Fatalf("activity_at = %d, want updated_at", c.ActivityAt)
	}
}

func TestAProjectsLastMessageIsTheUsersNewestPrompt(t *testing.T) {
	db := newStore(t)
	sync := func(id string, events []agent.Event) {
		t.Helper()
		err := db.Sync(session.Meta{Agent: agent.KindClaude, ID: id, Cwd: "/p",
			UpdatedAt: time.UnixMilli(10_000), SizeBytes: int64(len(events))}, events)
		if err != nil {
			t.Fatal(err)
		}
	}
	lastMessage := func() int64 {
		t.Helper()
		activity, err := db.Activity()
		if err != nil {
			t.Fatal(err)
		}
		return activity["/p"].LastMessageAt
	}

	// The first sweep reads it off the transcript; the reply after it does not count.
	transcript := []agent.Event{
		{Kind: agent.EventUserMessage, Text: "one", At: 1_000},
		{Kind: agent.EventText, Text: "a", At: 8_000},
	}
	sync("a", transcript)
	// A chat with no prompt does not pull the folder back.
	sync("b", []agent.Event{{Kind: agent.EventText, Text: "b", At: 9_000}})
	if got := lastMessage(); got != 1_000 {
		t.Fatalf("after sync: last_message_at = %d, want the prompt", got)
	}

	// A send is heard as its turn starting, before the transcript has it.
	sent := time.UnixMilli(12_000)
	if err := db.SetStatus("claude:a", "/p", agent.ChatWorking, sent,
		TurnObservation{Key: ManagerTurnPrefix + "t2", StartedAt: &sent}); err != nil {
		t.Fatal(err)
	}
	if got := lastMessage(); got != 12_000 {
		t.Fatalf("after send: last_message_at = %d, want the send", got)
	}

	// A sweep that has not caught up with the send does not undo it.
	sync("a", append(transcript, agent.Event{Kind: agent.EventText, Text: "more", At: 11_000}))
	if got := lastMessage(); got != 12_000 {
		t.Fatalf("after a stale sweep: last_message_at = %d, want the send", got)
	}

	// The project row carries it.
	if _, err := db.SyncProjects([]Project{{Path: "/p", Kind: "folder", LastMessageAt: lastMessage()}}); err != nil {
		t.Fatal(err)
	}
	projects, err := db.projects(``)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].LastMessageAt != 12_000 {
		t.Fatalf("project row = %+v, want last_message_at 12000", projects)
	}
}
