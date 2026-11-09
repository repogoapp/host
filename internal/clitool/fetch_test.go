package clitool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRelinkReplacesABinaryWithALink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "bin", "codex")
	os.MkdirAll(filepath.Dir(link), 0o755)
	os.WriteFile(link, []byte("old binary"), 0o755)
	target := filepath.Join(dir, "share", "v2", "bin", "codex")

	if err := relink(link, target); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.Readlink(link); got != target {
		t.Fatalf("link points at %q", got)
	}
	if _, err := os.Lstat(link + ".new"); err == nil {
		t.Fatal("the temporary link was left behind")
	}
}

func TestPruneKeepsCurrentAndPrevious(t *testing.T) {
	parent := t.TempDir()
	for _, v := range []string{"v1", "v2", "v3", ".v4-tmp"} {
		os.MkdirAll(filepath.Join(parent, v, "bin"), 0o755)
	}
	prune(parent, filepath.Join(parent, "v3"), filepath.Join(parent, "v2", "bin", "codex"))

	for v, want := range map[string]bool{"v1": false, "v2": true, "v3": true, ".v4-tmp": true} {
		if _, err := os.Stat(filepath.Join(parent, v)); (err == nil) != want {
			t.Fatalf("%s kept=%v, want %v", v, err == nil, want)
		}
	}
}
