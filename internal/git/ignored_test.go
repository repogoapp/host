package git_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/git"
)

// The watcher drops what this reports, so a wrong answer either floods the
// phone with build output or hides a real edit.
func TestIgnoredFollowsGitignoreNegationAndTracking(t *testing.T) {
	svc, dir := repo(t)
	write(t, dir, ".gitignore", "build/\n*.log\n!keep.log\n")
	write(t, dir, "tracked.log", "tracked before the rule\n")
	run(t, dir, "add", "-f", "tracked.log")
	commit(t, dir, "ignore rules")

	got, err := svc.Ignored(context.Background(), dir, []string{
		"build/", "build/out/app.o", "src/main.go", "debug.log", "keep.log", "tracked.log", "a b.log",
	})
	if err != nil {
		t.Fatalf("ignored: %v", err)
	}
	want := map[string]bool{"build/": true, "build/out/app.o": true, "debug.log": true, "a b.log": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ignored = %v, want %v", got, want)
	}
}

func TestIgnoredInAPlainFolderIsNothing(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".gitignore"), []byte("build/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := git.New(files.New(files.Config{Roots: files.StaticRoots{base}}), slog.New(slog.DiscardHandler))

	got, err := svc.Ignored(context.Background(), base, []string{"build/"})
	if err != nil {
		t.Fatalf("ignored: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("a plain folder ignored %v", got)
	}
}

func TestIgnoredRefusesAPathOutsideEveryProject(t *testing.T) {
	svc, _ := repo(t)
	if _, err := svc.Ignored(context.Background(), t.TempDir(), []string{"x"}); err == nil {
		t.Error("a folder outside every project was checked")
	}
}
