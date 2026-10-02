package git_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/repogo/host/internal/git"
)

func TestRootDirAndMainCheckoutReadALinkedWorktree(t *testing.T) {
	_, dir := repo(t)
	dir, _ = filepath.EvalSymlinks(dir)
	linked := filepath.Join(t.TempDir(), "linked")
	gitCmd(t, dir, "worktree", "add", "-b", "side", linked)
	linked, _ = filepath.EvalSymlinks(linked)
	sub := filepath.Join(linked, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	if got := git.Root(sub); got != linked {
		t.Errorf("Root(sub) = %s, want %s", got, linked)
	}
	if got, want := git.Dir(linked), filepath.Join(dir, ".git", "worktrees", "linked"); got != want {
		t.Errorf("Dir(linked) = %s, want %s", got, want)
	}
	if got := git.MainCheckout(linked); got != dir {
		t.Errorf("MainCheckout(linked) = %s, want %s", got, dir)
	}
	if got := git.MainCheckout(dir); got != dir {
		t.Errorf("MainCheckout(main) = %s, want itself", got)
	}
	if got := git.Root(t.TempDir()); got != "" {
		t.Errorf("Root(plain folder) = %s, want empty", got)
	}
}
