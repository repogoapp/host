package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
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

// The head fallback must retain its own date when a long turn pushes the
// newest turn_context outside the tail window.
func TestListKeepsModelAndDateTogether(t *testing.T) {
	for _, longTail := range []bool{false, true} {
		home := t.TempDir()
		dir := filepath.Join(home, "sessions")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		lines := []string{
			`{"type":"session_meta","payload":{"session_id":"t1","cwd":"/w"}}`,
			`{"type":"turn_context","timestamp":"2026-10-30T10:00:00Z","payload":{"model":"first"}}`,
			`{"type":"turn_context","timestamp":"2026-10-30T11:00:00Z","payload":{"model":"latest"}}`,
		}
		wantModel, hour := "latest", 11
		if longTail {
			lines = append(lines, strings.Repeat("{}\n", 150_000))
			wantModel, hour = "first", 10
		}
		path := filepath.Join(dir, "rollout-model.jsonl")
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		metas, err := NewSessions(home).List()
		if err != nil || len(metas) != 1 {
			t.Fatalf("List = %v, %v", metas, err)
		}
		want := time.Date(2026, 10, 30, hour, 0, 0, 0, time.UTC)
		if m := metas[0]; m.Model != wantModel || !m.ModelAt.Equal(want) {
			t.Fatalf("model = %q at %v; want %q at %v", m.Model, m.ModelAt, wantModel, want)
		}
	}
}
