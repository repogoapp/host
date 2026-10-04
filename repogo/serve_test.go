package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectsDirKeepsTheDisksSpelling(t *testing.T) {
	home := t.TempDir()
	if got, want := projectsDir(home), filepath.Join(home, "RepoGo"); got != want {
		t.Fatalf("no folder yet: got %q, want %q", got, want)
	}
	if err := os.Mkdir(filepath.Join(home, "repogo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := projectsDir(home), filepath.Join(home, "repogo"); got != want {
		t.Fatalf("lowercase folder: got %q, want %q", got, want)
	}
}
