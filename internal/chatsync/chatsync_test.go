package chatsync

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/agents/claude"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
)

func claudeLine(sessionID, text string) string {
	b, _ := json.Marshal(map[string]any{
		"type":      "user",
		"cwd":       "/srv/demo",
		"sessionId": sessionID,
		"message":   map[string]any{"role": "user", "content": text},
	})
	return string(b)
}

func writeSession(t *testing.T, home, sessionID, text string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "-srv-demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(claudeLine(sessionID, text)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The case the user hits: a chat started in a terminal after the app was
// already open. Pull-to-refresh runs a sweep, and the new session must appear
// without waiting for the periodic one.
func TestSweepPicksUpASessionCreatedLater(t *testing.T) {
	home := t.TempDir()
	sessions := session.NewStore(claude.NewSessions(filepath.Join(home, ".claude")))
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	writeSession(t, home, "first", "hello")
	syncer := New(sessions, db, slog.Default())
	if n := syncer.Once(context.Background()); n == 0 {
		t.Fatal("session discovery found nothing in the temporary home")
	}

	beforePage, err := db.Chats(store.ChatQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	before := beforePage.Chats

	// A brand-new chat, started while the app is running.
	writeSession(t, home, "second", "started in a terminal")
	syncer.Once(context.Background())

	afterPage, err := db.Chats(store.ChatQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	after := afterPage.Chats
	if len(after) != len(before)+1 {
		t.Fatalf("refresh did not pick up the new session: %d → %d", len(before), len(after))
	}

	var found bool
	for _, c := range after {
		if c.ID == "claude:second" {
			found = true
		}
	}
	if !found {
		t.Error("the newly created chat is missing from the list")
	}
}

// A turn the host runs writes its status through the Manager, not a hook. The
// store's listener hears the placeholder at once, and Refresh sweeps the file
// behind it so the listener hears the row again with its title.
func TestRefreshFillsTheRowBehindAStatus(t *testing.T) {
	home := t.TempDir()
	sessions := session.NewStore(claude.NewSessions(filepath.Join(home, ".claude")))
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	writeSession(t, home, "running", "fix the build")
	heard := make(chan store.Change, 4)
	db.Notify(func(c store.Change) { heard <- c })
	syncer := New(sessions, db, slog.Default())

	id := store.ChatID("claude:running")
	if err := db.SetStatus(id, "/srv/demo", agent.ChatWorking, time.Now(), store.TurnObservation{}); err != nil {
		t.Fatal(err)
	}
	if c := <-heard; len(c.Changed) != 1 || c.Changed[0] != id {
		t.Fatalf("the status was heard as %v, want %s", c, id)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go syncer.Run(ctx)
	syncer.Refresh(id)

	select {
	case c := <-heard:
		chat, err := db.Info(id)
		if err != nil || len(c.Changed) != 1 || c.Changed[0] != id || chat.Status != agent.ChatWorking || chat.Title == "" {
			t.Fatalf("heard %v; row %+v, %v: want %s working with a title", c, chat, err, id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Refresh did not sweep the transcript behind the status")
	}
}

// A turn the host runs lists its chat with the prompt's title and the
// requested model before any transcript exists.
func TestObserveTurnListsTitleAndModel(t *testing.T) {
	sessions := session.NewStore(claude.NewSessions(filepath.Join(t.TempDir(), ".claude")))
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	syncer := New(sessions, db, slog.Default())

	started := time.Now()
	syncer.ObserveTurn(agent.TurnStatus{TurnID: "t1", Agent: agent.KindClaude, SessionID: "s1", Cwd: "/srv/demo",
		Prompt: "fix the build", Model: "sonnet", StartedAt: &started}, agent.ChatWorking, started)
	chat, err := db.Info("claude:s1")
	if err != nil {
		t.Fatal(err)
	}
	if chat.Title != "fix the build" || chat.Model != "sonnet" {
		t.Fatalf("title %q model %q; want the turn's prompt and model", chat.Title, chat.Model)
	}
}

// A new chat's title reaches Claude's own file when its first turn ends, so
// `claude --resume` names it too; a rename during that turn is left alone.
func TestObserveTurnKeepsTheStartTitleInTheTranscript(t *testing.T) {
	home := t.TempDir()
	sessions := session.NewStore(claude.NewSessions(filepath.Join(home, ".claude")))
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	syncer := New(sessions, db, slog.Default())
	transcript := func(id string) string {
		b, err := os.ReadFile(filepath.Join(home, ".claude", "projects", "-srv-demo", id+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	for _, tc := range []struct {
		session, renamed string
		wantInFile       bool
	}{{"s1", "", true}, {"s2", "Mine", false}} {
		writeSession(t, home, tc.session, "can you fix the build please")
		started := time.Now()
		turn := agent.TurnStatus{TurnID: "t-" + tc.session, Agent: agent.KindClaude, SessionID: tc.session, Cwd: "/srv/demo",
			Prompt: "can you fix the build please", Title: "Fix the Build", StartedAt: &started}
		syncer.ObserveTurn(turn, agent.ChatWorking, started)
		if tc.renamed != "" {
			if err := db.SetTitle(store.ChatID("claude:"+tc.session), tc.renamed); err != nil {
				t.Fatal(err)
			}
		}
		ended := started.Add(time.Second)
		turn.EndedAt = &ended
		syncer.ObserveTurn(turn, agent.ChatIdle, ended)

		got := strings.Contains(transcript(tc.session), `"customTitle":"Fix the Build"`)
		if got != tc.wantInFile {
			t.Errorf("%s: title in Claude's file = %v, want %v", tc.session, got, tc.wantInFile)
		}
	}
}
