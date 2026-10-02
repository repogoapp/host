package store

import (
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
)

// A reparse that finds no model keeps the one already known: a truncated tail
// window is not a chat that stopped using one.
func TestChatRowCarriesItsModel(t *testing.T) {
	db := newStore(t)
	meta := session.Meta{Agent: agent.KindClaude, ID: "m", Cwd: "/w", UpdatedAt: time.UnixMilli(100), SizeBytes: 1, Model: "claude-opus-5-5"}
	events := []agent.Event{{Kind: agent.EventText, Text: "ok"}}
	if err := db.Sync(meta, events); err != nil {
		t.Fatal(err)
	}
	chat, err := db.Info("claude:m")
	if err != nil {
		t.Fatal(err)
	}
	if chat.Model != "claude-opus-5-5" {
		t.Fatalf("model = %q, want claude-opus-5-5", chat.Model)
	}

	meta.Model, meta.SizeBytes = "", 2
	if err := db.Sync(meta, events); err != nil {
		t.Fatal(err)
	}
	if chat, _ = db.Info("claude:m"); chat.Model != "claude-opus-5-5" {
		t.Fatalf("after a reparse with no model, model = %q, want it kept", chat.Model)
	}
}

// Settings read from the transcript reach the chat row, and a reparse that
// finds none keeps them, as it does the model.
func TestChatRowCarriesItsSettings(t *testing.T) {
	db := newStore(t)
	fast := true
	meta := session.Meta{Agent: agent.KindCodex, ID: "s", Cwd: "/w", UpdatedAt: time.UnixMilli(100), SizeBytes: 1,
		Settings: session.Settings{PermissionMode: agent.PermissionFullAccess, Mode: "plan", ReasoningLevel: "high", FastMode: &fast}}
	events := []agent.Event{{Kind: agent.EventText, Text: "ok"}}
	for _, size := range []int64{1, 2} {
		meta.SizeBytes = size
		if err := db.Sync(meta, events); err != nil {
			t.Fatal(err)
		}
		chat, err := db.Info("codex:s")
		if err != nil {
			t.Fatal(err)
		}
		if chat.PermissionMode != "full-access" || chat.Mode != "plan" || chat.ReasoningLevel != "high" || chat.FastMode == nil || !*chat.FastMode {
			t.Fatalf("after sync %d, chat = %+v", size, chat)
		}
		meta.Settings = session.Settings{}
	}
}

// Write 1 of a turn (queries.go) lists a new chat with its title and model, so
// its card fills in at once. The transcript then wins: its model replaces the
// requested one, and a request never overwrites what the transcript named.
func TestTurnStartListsTitleAndModel(t *testing.T) {
	db := newStore(t)
	heard := listen(db)
	id := ChatID("claude:new")
	started := time.UnixMilli(1_000)
	sent := TurnObservation{Key: ManagerTurnPrefix + "t1", StartedAt: &started,
		Prompt: "fix the login bug on the settings page", Model: "opus"}
	if err := db.SetStatus(id, "/w", agent.ChatWorking, started, sent); err != nil {
		t.Fatal(err)
	}
	one(t, heard, Change{Changed: []ChatID{id}})
	chat, err := db.Info(id)
	if err != nil {
		t.Fatal(err)
	}
	if chat.Title != "fix the login bug on the settings page" || chat.Model != "opus" {
		t.Fatalf("after write 1, title %q model %q; want the prompt's words and opus", chat.Title, chat.Model)
	}

	// Write 2: the file has no user line yet, so the title stays.
	meta := session.Meta{Agent: agent.KindClaude, ID: "new", Cwd: "/w", UpdatedAt: time.UnixMilli(1_100), SizeBytes: 1}
	if err := db.Sync(meta, nil); err != nil {
		t.Fatal(err)
	}
	if chat, _ = db.Info(id); chat.Title != "fix the login bug on the settings page" || chat.Model != "opus" {
		t.Fatalf("after an empty transcript, title %q model %q; want both kept", chat.Title, chat.Model)
	}

	// Write 3: the reply names the model the provider ran.
	meta.Model, meta.SizeBytes = "claude-opus-5-5", 2
	events := []agent.Event{{Kind: agent.EventUserMessage, Text: "fix the login bug on the settings page"}, {Kind: agent.EventText, Text: "done"}}
	if err := db.Sync(meta, events); err != nil {
		t.Fatal(err)
	}
	if chat, _ = db.Info(id); chat.Title != "fix the login bug on the settings page" || chat.Model != "claude-opus-5-5" {
		t.Fatalf("after the reply, title %q model %q; want the transcript's model", chat.Title, chat.Model)
	}

	// The next turn asks for another model; the row keeps the transcript's
	// until the transcript shows the change.
	next := time.UnixMilli(2_000)
	if err := db.SetStatus(id, "/w", agent.ChatWorking, next, TurnObservation{Key: ManagerTurnPrefix + "t2", StartedAt: &next,
		Prompt: "now the tests", Model: "sonnet"}); err != nil {
		t.Fatal(err)
	}
	if chat, _ = db.Info(id); chat.Title != "fix the login bug on the settings page" || chat.Model != "claude-opus-5-5" {
		t.Fatalf("after a later turn, title %q model %q; want both kept", chat.Title, chat.Model)
	}
}

// Claude's `default` alias would show as "Default (recommended)"; the row
// waits for the transcript to name the real model instead.
func TestTurnStartSkipsTheDefaultAlias(t *testing.T) {
	db := newStore(t)
	id := ChatID("claude:d")
	started := time.UnixMilli(1_000)
	if err := db.SetStatus(id, "/w", agent.ChatWorking, started, TurnObservation{Key: ManagerTurnPrefix + "t1", StartedAt: &started,
		Prompt: "hello", Model: "default"}); err != nil {
		t.Fatal(err)
	}
	if chat, _ := db.Info(id); chat.Title != "hello" || chat.Model != "" {
		t.Fatalf("title %q model %q; want hello and no model", chat.Title, chat.Model)
	}
}

// The row names the permission a turn runs with from write 1: Claude records
// it only on the prompt line, which a long turn pushes out of the tail read.
func TestTurnStartSetsThePermission(t *testing.T) {
	db := newStore(t)
	id := ChatID("claude:p")
	started := time.UnixMilli(1_000)
	if err := db.SetStatus(id, "/w", agent.ChatWorking, started, TurnObservation{Key: ManagerTurnPrefix + "t1", StartedAt: &started,
		Prompt: "hello", PermissionMode: agent.PermissionAutoAcceptEdits}); err != nil {
		t.Fatal(err)
	}
	if chat, _ := db.Info(id); chat.PermissionMode != string(agent.PermissionAutoAcceptEdits) {
		t.Fatalf("after write 1, permission %q; want auto-accept-edits", chat.PermissionMode)
	}

	// A transcript read that misses the prompt line keeps it.
	meta := session.Meta{Agent: agent.KindClaude, ID: "p", Cwd: "/w", UpdatedAt: time.UnixMilli(1_100), SizeBytes: 1}
	if err := db.Sync(meta, nil); err != nil {
		t.Fatal(err)
	}
	// So does the turn's end, which carries no permission.
	ended := time.UnixMilli(1_200)
	if err := db.SetStatus(id, "/w", agent.ChatIdle, ended, TurnObservation{Key: ManagerTurnPrefix + "t1", StartedAt: &started, FinishedAt: &ended}); err != nil {
		t.Fatal(err)
	}
	if chat, _ := db.Info(id); chat.PermissionMode != string(agent.PermissionAutoAcceptEdits) {
		t.Fatalf("after the transcript and the turn's end, permission %q; want it kept", chat.PermissionMode)
	}

	// The next turn's pick replaces it.
	next := time.UnixMilli(2_000)
	if err := db.SetStatus(id, "/w", agent.ChatWorking, next, TurnObservation{Key: ManagerTurnPrefix + "t2", StartedAt: &next,
		Prompt: "go", PermissionMode: agent.PermissionFullAccess}); err != nil {
		t.Fatal(err)
	}
	if chat, _ := db.Info(id); chat.PermissionMode != string(agent.PermissionFullAccess) {
		t.Fatalf("after the next turn, permission %q; want full-access", chat.PermissionMode)
	}
}

// The title a device starts a chat with outlives the transcript's opening
// words; a title the agent's own files name (a rename) replaces it.
func TestStartTitleStaysUntilTheAgentNamesOne(t *testing.T) {
	db := newStore(t)
	id := ChatID("claude:new")
	started := time.UnixMilli(1_000)
	if err := db.SetStatus(id, "/w", agent.ChatWorking, started, TurnObservation{Key: ManagerTurnPrefix + "t1", StartedAt: &started,
		Prompt: "can you fix the login bug please", Title: "Fix Login Bug"}); err != nil {
		t.Fatal(err)
	}
	if chat, _ := db.Info(id); chat.Title != "Fix Login Bug" {
		t.Fatalf("after write 1, title %q; want the device's", chat.Title)
	}

	meta := session.Meta{Agent: agent.KindClaude, ID: "new", Cwd: "/w", UpdatedAt: time.UnixMilli(1_100), SizeBytes: 1}
	events := []agent.Event{{Kind: agent.EventUserMessage, Text: "can you fix the login bug please"}}
	if err := db.Sync(meta, events); err != nil {
		t.Fatal(err)
	}
	if chat, _ := db.Info(id); chat.Title != "Fix Login Bug" {
		t.Fatalf("after the transcript, title %q; want the device's kept over the opening words", chat.Title)
	}

	meta.Title, meta.SizeBytes = "Login fix", 2
	if err := db.Sync(meta, events); err != nil {
		t.Fatal(err)
	}
	if chat, _ := db.Info(id); chat.Title != "Login fix" {
		t.Fatalf("after a rename in the transcript, title %q; want the rename", chat.Title)
	}
}
