package agents_test

import (
	"path/filepath"
	"testing"

	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/agents/codex"
)

// The inbox row names the model of the newest request: a switched model shows
// the new one, and neither the CLI's own "<synthetic>" reply nor a subagent's
// sidechain row counts.
func TestClaudeListNamesTheNewestModel(t *testing.T) {
	home := t.TempDir()
	reply := func(model string, sidechain bool) map[string]any {
		return map[string]any{"type": "assistant", "isSidechain": sidechain,
			"message": map[string]any{"role": "assistant", "model": model, "content": []map[string]any{{"type": "text", "text": "ok"}}}}
	}
	jsonl(t, filepath.Join(home, "projects", "-w", "s1.jsonl"),
		map[string]any{"type": "user", "cwd": "/w", "message": map[string]any{"role": "user", "content": "hi"}},
		reply("claude-sonnet-5", false),
		reply("claude-opus-5-5", false),
		reply("claude-haiku-4-5-20251001", true),
		reply("<synthetic>", false),
	)

	metas, err := claude.NewSessions(home).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].Model != "claude-opus-5-5" {
		t.Fatalf("listed %+v, want model claude-opus-5-5", metas)
	}
}

// Codex restates the model on every turn_context; the newest segment's newest
// turn wins.
func TestCodexListNamesTheNewestModel(t *testing.T) {
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "09", "24")
	id := "019f-model"
	meta := map[string]any{"type": "session_meta", "payload": map[string]any{"session_id": id, "id": id, "cwd": "/w", "source": "cli"}}
	turn := func(model string) map[string]any {
		return map[string]any{"type": "turn_context", "payload": map[string]any{"cwd": "/w", "model": model}}
	}
	jsonl(t, filepath.Join(day, "rollout-2026-09-24T10-00-00-"+id+".jsonl"), meta, turn("gpt-5.6-sol"))

	c := codex.NewSessions(home)
	metas, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].Model != "gpt-5.6-sol" {
		t.Fatalf("listed %+v, want model gpt-5.6-sol", metas)
	}

	jsonl(t, filepath.Join(day, "rollout-2026-09-24T10-00-00-"+id+".jsonl"), meta, turn("gpt-5.6-sol"), turn("gpt-6-astra"))
	metas, err = c.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].Model != "gpt-6-astra" {
		t.Fatalf("listed %+v, want model gpt-6-astra", metas)
	}
}
