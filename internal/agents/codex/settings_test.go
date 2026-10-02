package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogo/host/internal/agent"
)

// A thread's settings come from its newest turn_context and
// thread_settings_applied lines.
func TestListReadsTurnSettings(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "09", "26")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"session_meta","payload":{"session_id":"t1","cwd":"/w"}}`,
		`{"type":"turn_context","payload":{"model":"gpt-6-astra","effort":"low","sandbox_policy":{"type":"read-only"},"collaboration_mode":{"mode":"default"}}}`,
		`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"service_tier":"priority"}}}`,
		`{"type":"turn_context","payload":{"model":"gpt-6-astra","effort":"ultra","sandbox_policy":{"type":"workspace-write"},"collaboration_mode":{"mode":"plan"}}}`,
	}
	path := filepath.Join(dir, "rollout-2026-09-26T10-00-00-t1.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	metas, err := NewSessions(home).List()
	if err != nil || len(metas) != 1 {
		t.Fatalf("List = %v, %v", metas, err)
	}
	got := metas[0].Settings
	if got.PermissionMode != agent.PermissionAutoAcceptEdits || got.Mode != "plan" || got.ReasoningLevel != "ultra" {
		t.Fatalf("settings = %+v, want auto-accept-edits, plan, ultra", got)
	}
	if got.FastMode == nil || !*got.FastMode {
		t.Fatalf("fast mode = %v, want true", got.FastMode)
	}
}
