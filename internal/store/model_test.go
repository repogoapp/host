package store

import (
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
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
// its card fills in at once.
func TestTurnStartListsTitleAndModel(t *testing.T) {
	db := newStore(t)
	heard := listen(db)
	id := ChatID("claude:new")
	started := time.UnixMilli(1_000)
	sent := TurnObservation{Key: ManagerTurnPrefix + "t1", StartedAt: &started,
		Prompt: "fix the login bug on the settings page", Model: "claude-opus-5-5"}
	if err := db.SetStatus(id, "/w", agent.ChatWorking, started, sent); err != nil {
		t.Fatal(err)
	}
	one(t, heard, Change{Changed: []ChatID{id}})
	chat, err := db.Info(id)
	if err != nil {
		t.Fatal(err)
	}
	if chat.Title != "fix the login bug on the settings page" || chat.Model != "claude-opus-5-5" {
		t.Fatalf("after write 1, title %q model %q; want the prompt's words and the model", chat.Title, chat.Model)
	}

	// Write 2: the file has no user line yet, so the title stays.
	meta := session.Meta{Agent: agent.KindClaude, ID: "new", Cwd: "/w", UpdatedAt: time.UnixMilli(1_100), SizeBytes: 1}
	if err := db.Sync(meta, nil); err != nil {
		t.Fatal(err)
	}
	if chat, _ = db.Info(id); chat.Title != "fix the login bug on the settings page" || chat.Model != "claude-opus-5-5" {
		t.Fatalf("after an empty transcript, title %q model %q; want both kept", chat.Title, chat.Model)
	}
}

// A turn's model holds through stale sweeps and an interruption without a
// model-bearing row. A later terminal turn can replace it.
func TestTurnModelHoldsThroughStaleTranscript(t *testing.T) {
	db := newStore(t)
	id := ChatID("codex:m")
	meta := session.Meta{Agent: agent.KindCodex, ID: "m", Cwd: "/w", UpdatedAt: time.UnixMilli(500), SizeBytes: 1, Model: "gpt-6", ModelLabel: "GPT-6"}
	old := []agent.Event{{Kind: agent.EventUserMessage, Text: "first", At: 100}, {Kind: agent.EventText, Text: "ok", At: 200}}
	if err := db.Sync(meta, old); err != nil {
		t.Fatal(err)
	}
	model := func(when string, want string) {
		t.Helper()
		if chat, _ := db.Info(id); chat.Model != want {
			t.Fatalf("%s, model %q; want %q", when, chat.Model, want)
		}
	}
	model("after the first sweep", "gpt-6")

	started := time.UnixMilli(1_000)
	if err := db.SetStatus(id, "/w", agent.ChatWorking, started, TurnObservation{Key: ManagerTurnPrefix + "t2", StartedAt: &started,
		Prompt: "next", Model: "gpt-6-astra"}); err != nil {
		t.Fatal(err)
	}
	model("after the switch", "gpt-6-astra")
	if chat, _ := db.Info(id); chat.ModelLabel != "" {
		t.Fatalf("after the switch, label %q; want the old model's dropped", chat.ModelLabel)
	}

	meta.SizeBytes = 2
	if err := db.Sync(meta, old); err != nil {
		t.Fatal(err)
	}
	model("after a stale sweep mid-turn", "gpt-6-astra")

	// The agent's own hook during the host's turn is that turn seen late; the
	// model it names does not replace the host's.
	if err := db.SetStatus(id, "/w", agent.ChatWorking, started, TurnObservation{Key: "codex:hook-1", Model: "gpt-6"}); err != nil {
		t.Fatal(err)
	}
	model("after a late hook mid-turn", "gpt-6-astra")

	ended := time.UnixMilli(2_000)
	if err := db.SetStatus(id, "/w", agent.ChatIdle, ended, TurnObservation{Key: ManagerTurnPrefix + "t2", StartedAt: &started, FinishedAt: &ended}); err != nil {
		t.Fatal(err)
	}
	meta.SizeBytes = 3
	if err := db.Sync(meta, append(old, agent.Event{Kind: agent.EventUserMessage, Text: "next", At: 1_000})); err != nil {
		t.Fatal(err)
	}
	model("after a sweep once it ended", "gpt-6-astra")

	meta.SizeBytes, meta.Model = 4, "gpt-6-mini"
	later := append(old, agent.Event{Kind: agent.EventUserMessage, Text: "next", At: 1_000},
		agent.Event{Kind: agent.EventUserMessage, Text: "from the terminal", At: 3_000})
	if err := db.Sync(meta, later); err != nil {
		t.Fatal(err)
	}
	model("after a later turn from a terminal", "gpt-6-mini")
}

// A model-bearing row corrects the live estimate without a later activity or
// completion report taking it back, including when the estimate arrives late.
func TestTranscriptModelReleasesLiveEstimate(t *testing.T) {
	for _, afterEnd := range []bool{false, true} {
		for _, initialModel := range []string{"", "requested"} {
			db := newStore(t)
			id := ChatID("claude:model")
			started, ended := time.UnixMilli(1_000), time.UnixMilli(2_000)
			turn := TurnObservation{Key: ManagerTurnPrefix + "t1", StartedAt: &started, Model: initialModel}
			status := func(activity agent.ChatStatus, at time.Time) {
				t.Helper()
				if err := db.SetStatus(id, "/w", activity, at, turn); err != nil {
					t.Fatal(err)
				}
			}
			check := func(want, label string) {
				t.Helper()
				chat, err := db.Info(id)
				if err != nil || chat.Model != want || chat.ModelLabel != label {
					t.Fatalf("initial %q, after end %v: model row %+v, err %v; want %q / %q", initialModel, afterEnd, chat, err, want, label)
				}
			}
			status(agent.ChatWorking, started)
			turn.Model = "requested"
			status(agent.ChatWorking, started.Add(time.Millisecond))
			check("requested", "")
			if afterEnd {
				turn.FinishedAt = &ended
				status(agent.ChatIdle, ended)
			}
			meta := session.Meta{Agent: agent.KindClaude, ID: "model", Cwd: "/w", Model: "actual", ModelLabel: "Actual",
				UpdatedAt: time.UnixMilli(3_000)}
			events := []agent.Event{{Kind: agent.EventUserMessage, At: 1_000}, {Kind: agent.EventText, Text: "reply", At: 1_100}}
			sync := func(at time.Time) {
				t.Helper()
				meta.ModelAt = at
				meta.SizeBytes++
				if err := db.Sync(meta, events); err != nil {
					t.Fatal(err)
				}
			}
			// Fresh output and file dates do not make an old or undated model fresh.
			sync(time.Time{})
			check("requested", "")
			sync(time.UnixMilli(200))
			check("requested", "")
			heard := listen(db)
			sync(time.UnixMilli(1_100))
			check("actual", "Actual")
			one(t, heard, Change{Changed: []ChatID{id}})
			meta.Model, meta.ModelLabel = "old", "Old"
			sync(time.UnixMilli(200))
			check("actual", "Actual")
			status(agent.ChatWorking, time.UnixMilli(3_100))
			check("actual", "Actual")
			turn.FinishedAt = &ended
			status(agent.ChatIdle, time.UnixMilli(3_200))
			check("actual", "Actual")

			next := time.UnixMilli(4_000)
			turn = TurnObservation{Key: ManagerTurnPrefix + "t2", StartedAt: &next, Model: "next"}
			status(agent.ChatWorking, next)
			check("next", "")
			sync(time.UnixMilli(1_100))
			check("next", "")
		}
	}
}

// A newer file can be a deliberate rewind, whose older model must replace
// the reading from the removed part of the conversation.
func TestRewindRestoresOlderModel(t *testing.T) {
	db := newStore(t)
	meta := session.Meta{Agent: agent.KindCodex, ID: "rewind", Cwd: "/w", Model: "new",
		ModelAt: time.UnixMilli(2_000), UpdatedAt: time.UnixMilli(3_000), SizeBytes: 2}
	events := []agent.Event{{Kind: agent.EventText, Text: "first"}, {Kind: agent.EventText, Text: "second"}}
	if err := db.Sync(meta, events); err != nil {
		t.Fatal(err)
	}
	meta.Model, meta.ModelAt = "old", time.UnixMilli(1_000)
	meta.UpdatedAt, meta.SizeBytes = time.UnixMilli(4_000), 1
	if err := db.Sync(meta, events[:1]); err != nil {
		t.Fatal(err)
	}
	chat, err := db.Info("codex:rewind")
	if err != nil || chat.Model != "old" {
		t.Fatalf("rewound chat %+v, err %v; want old model", chat, err)
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
