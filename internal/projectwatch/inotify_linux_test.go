package projectwatch

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/repogoapp/host/internal/testwait"
)

// seen collects what a watcher reports: each path, and whether any report
// said it was created.
type seen struct {
	mu    sync.Mutex
	paths map[string]bool
	lost  int
}

func (s *seen) add(path string, created bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paths == nil {
		s.paths = map[string]bool{}
	}
	s.paths[path] = s.paths[path] || created
}

func (s *seen) lose() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lost++
}

func (s *seen) has(path string) (created, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	created, ok = s.paths[path]
	return created, ok
}

func (s *seen) waitFor(t *testing.T, path string) (created bool) {
	t.Helper()
	testwait.For(t, path, func() bool { _, ok := s.has(path); return ok })
	created, _ = s.has(path)
	return created
}

// watchDir watches a temporary tree, skipping folders named "skipped".
func watchDir(t *testing.T) (root string, got *seen) {
	t.Helper()
	root = resolved(t.TempDir())
	got = &seen{}
	keep := func(dir string) bool { return filepath.Base(dir) != "skipped" }
	stop, ok := watchEvents([]string{root}, keep, got.add, got.lose)
	if !ok {
		t.Fatal("no watcher")
	}
	t.Cleanup(stop)
	return root, got
}

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestInotifyReportsFilesMadeEditedAndRemoved(t *testing.T) {
	root := resolved(t.TempDir())
	old := filepath.Join(root, "src", "old.go")
	write(t, old)
	got := &seen{}
	stop, ok := watchEvents([]string{root}, func(string) bool { return true }, got.add, got.lose)
	if !ok {
		t.Fatal("no watcher")
	}
	defer stop()

	// A folder there before the watch is watched too.
	if err := os.WriteFile(old, []byte("edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got.waitFor(t, old) {
		t.Error("an edit read as created")
	}
	made := filepath.Join(root, "src", "new.go")
	write(t, made)
	if !got.waitFor(t, made) {
		t.Error("a new file did not read as created")
	}
	gone := filepath.Join(root, "top.txt")
	write(t, gone)
	got.waitFor(t, gone)
	got.mu.Lock()
	got.paths = nil
	got.mu.Unlock()
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if got.waitFor(t, gone) {
		t.Error("a removal read as created")
	}
}

// A folder made after the watch started is watched, and the files that landed
// in it before its watch did are still reported.
func TestInotifyFollowsNewFolders(t *testing.T) {
	root, got := watchDir(t)
	first := filepath.Join(root, "a", "b", "first.go")
	write(t, first)
	if !got.waitFor(t, first) {
		t.Error("a file in a new folder did not read as created")
	}
	if !got.waitFor(t, filepath.Join(root, "a")) {
		t.Error("the new folder did not read as created")
	}
	later := filepath.Join(root, "a", "b", "later.go")
	write(t, later)
	got.waitFor(t, later)
}

func TestInotifySkipsFoldersNotKept(t *testing.T) {
	root, got := watchDir(t)
	write(t, filepath.Join(root, "skipped", "deep", "build.js"))
	marker := filepath.Join(root, "marker")
	write(t, marker)
	got.waitFor(t, marker)
	if _, ok := got.has(filepath.Join(root, "skipped", "deep", "build.js")); ok {
		t.Error("a file under a skipped folder was reported")
	}
}

// A renamed folder reports under its new name, never its old one.
func TestInotifyFollowsARenamedFolder(t *testing.T) {
	root, got := watchDir(t)
	before, after := filepath.Join(root, "before"), filepath.Join(root, "after")
	write(t, filepath.Join(before, "kept.go"))
	got.waitFor(t, filepath.Join(before, "kept.go"))
	if err := os.Rename(before, after); err != nil {
		t.Fatal(err)
	}
	got.waitFor(t, after)
	write(t, filepath.Join(after, "added.go"))
	got.waitFor(t, filepath.Join(after, "added.go"))
	if _, ok := got.has(filepath.Join(before, "added.go")); ok {
		t.Error("a file in the renamed folder was reported under its old name")
	}
}

// When the kernel drops events, the caller is told, and folders made
// meanwhile are picked up.
func TestInotifyOverflowReportsLostAndRescans(t *testing.T) {
	root := resolved(t.TempDir())
	got := &seen{}
	w, ok := openInotify([]string{root}, func(string) bool { return true }, got.add, got.lose)
	if !ok {
		t.Fatal("no watcher")
	}
	defer w.file.Close()
	missed := filepath.Join(root, "missed")
	if err := os.Mkdir(missed, 0o755); err != nil {
		t.Fatal(err)
	}

	w.handle(-1, unix.IN_Q_OVERFLOW, "")
	if got.lost != 1 {
		t.Errorf("lost = %d, want 1", got.lost)
	}
	watched := false
	for _, dir := range w.dirs {
		watched = watched || dir == missed
	}
	if !watched {
		t.Error("a folder made while events were dropped is not watched")
	}
}
