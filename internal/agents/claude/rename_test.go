package claude

import (
	"os"
	"path/filepath"
	"testing"
)

// A rename is read back as the session's title, over the one Claude generated.
func TestRenameIsTheListedTitle(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "projects", "-w")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","cwd":"/w","message":{"content":"hi"}}` + "\n" + `{"type":"ai-title","aiTitle":"Generated"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions := NewSessions(home)
	metas, err := sessions.List()
	if err != nil || len(metas) != 1 {
		t.Fatalf("List = %v, %v", metas, err)
	}
	if err := sessions.Rename(metas[0], "Mine"); err != nil {
		t.Fatal(err)
	}
	metas, err = sessions.List()
	if err != nil || len(metas) != 1 || metas[0].Title != "Mine" {
		t.Fatalf("List after rename = %+v, %v", metas, err)
	}
}
