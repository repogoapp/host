package codex

import (
	"os"
	"path/filepath"
	"testing"
)

// A rename is the newest index row for the thread, so it wins over the old name.
func TestRenameIsTheListedTitle(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "sessions", "2026", "09", "29")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rollout := `{"type":"session_meta","payload":{"session_id":"t1","cwd":"/w"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-09-29T10-00-00-t1.jsonl"), []byte(rollout), 0o644); err != nil {
		t.Fatal(err)
	}
	index := `{"id":"t1","thread_name":"Old","updated_at":"2026-09-29T10:00:00Z"}` + "\n"
	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions := NewSessions(home)
	metas, err := sessions.List()
	if err != nil || len(metas) != 1 || metas[0].Title != "Old" {
		t.Fatalf("List = %+v, %v", metas, err)
	}
	if err := sessions.Rename(metas[0], "Mine"); err != nil {
		t.Fatal(err)
	}
	metas, err = sessions.List()
	if err != nil || len(metas) != 1 || metas[0].Title != "Mine" {
		t.Fatalf("List after rename = %+v, %v", metas, err)
	}
}
