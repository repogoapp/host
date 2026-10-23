package chatsync

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/repogoapp/host/internal/agents/claude"
	"github.com/repogoapp/host/internal/session"
	"github.com/repogoapp/host/internal/store"
)

// A sweep brings a chat's subagent into the cache beside it, never as a chat;
// a later write to the subagent's own file reaches the cache through
// SyncChildren, and a pass with nothing new reports nothing.
func TestSweepSyncsAChatsSubagents(t *testing.T) {
	home := t.TempDir()
	writeSession(t, home, "parent", "look into it")
	subs := filepath.Join(home, ".claude", "projects", "-srv-demo", "parent", "subagents")
	if err := os.MkdirAll(subs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subs, "agent-a9.meta.json"), []byte(`{"agentType":"Explore","toolUseId":"toolu_task"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(subs, "agent-a9.jsonl")
	appendLine := func(text string) {
		f, err := os.OpenFile(transcript, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"` + text + `"}]}}` + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	appendLine("Looking.")

	sessions := session.NewStore(claude.NewSessions(filepath.Join(home, ".claude")))
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	syncer := New(sessions, db, slog.Default())
	syncer.Once(context.Background())

	page, err := db.Chats(store.ChatQuery{Limit: 10})
	if err != nil || len(page.Chats) != 1 {
		t.Fatalf("chats = %+v, %v; want only the parent", page.Chats, err)
	}
	if _, rows, err := db.SubagentMessages("claude:parent", "toolu_task", ""); err != nil || len(rows) != 1 || rows[0].Text != "Looking." {
		t.Fatalf("after the sweep: %+v, %v", rows, err)
	}

	metas, _ := sessions.List()
	appendLine("Found it.")
	changed, err := syncer.SyncChildren(metas[0])
	if err != nil || !slices.Equal(changed, []string{"toolu_task"}) {
		t.Fatalf("after a write = %v, %v; want toolu_task", changed, err)
	}
	if _, rows, _ := db.SubagentMessages("claude:parent", "toolu_task", ""); len(rows) != 2 || rows[1].Text != "Found it." {
		t.Fatalf("rows = %+v, want both lines", rows)
	}
	if changed, err := syncer.SyncChildren(metas[0]); err != nil || len(changed) != 0 {
		t.Fatalf("an idle pass = %v, %v; want nothing", changed, err)
	}
}
