package agents_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/session"
)

func TestDeleteRemovesTheProviderFile(t *testing.T) {
	home := t.TempDir()
	path := writeFile(t, filepath.Join(home, "projects", "-Users-me-project", "sess-1.jsonl"),
		`{"type":"user","sessionId":"sess-1","cwd":"/Users/me/project","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}`+"\n")

	s := session.NewStore(claude.NewSessions(home), codex.NewSessions(filepath.Join(home, "codex")))
	if _, _, err := s.Find("sess-1"); err != nil {
		t.Fatalf("fixture not listed: %v", err)
	}
	if err := s.Delete("sess-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file after delete: %v", err)
	}
	if err := s.Delete("sess-1"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("second delete = %v, want not found", err)
	}
}
