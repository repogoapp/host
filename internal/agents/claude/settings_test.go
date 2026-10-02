package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogo/host/internal/agent"
)

// A chat's settings come back from its transcript, newest winning, with plan
// read as the mode and the permission it replaced kept.
func TestListReadsTurnSettings(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "projects", "-w")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		`{"type":"user","cwd":"/w","permissionMode":"default","message":{"content":"hi"}}`,
		`{"type":"assistant","effort":"low","message":{"model":"claude-opus-5-5","usage":{"speed":"fast"}}}`,
		`{"type":"permission-mode","permissionMode":"bypassPermissions"}`,
		`{"type":"user","permissionMode":"plan","message":{"content":"plan it"}}`,
		`{"type":"assistant","effort":"xhigh","message":{"model":"claude-opus-5-5","usage":{"speed":"standard"}}}`,
		`{"type":"assistant","isSidechain":true,"effort":"max","message":{"model":"claude-opus-5-5","usage":{"speed":"fast"}}}`,
		`{"type":"permission-mode","isSidechain":true,"permissionMode":"acceptEdits"}`,
	}
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	metas, err := NewSessions(home).List()
	if err != nil || len(metas) != 1 {
		t.Fatalf("List = %v, %v", metas, err)
	}
	got := metas[0].Settings
	if got.PermissionMode != agent.PermissionFullAccess || got.Mode != "plan" || got.ReasoningLevel != "xhigh" {
		t.Fatalf("settings = %+v, want full-access, plan, xhigh", got)
	}
	if got.FastMode == nil || *got.FastMode {
		t.Fatalf("fast mode = %v, want false", got.FastMode)
	}
}
