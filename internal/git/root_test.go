package git_test

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/git"
)

// The root names the repository: later commits and a new remote leave it
// alone, an empty repository has none yet, and a shallow clone's oldest
// commit is not one.
func TestRootCommitIsTheFirstCommit(t *testing.T) {
	_, dir := repo(t)
	first := revParse(t, dir, "HEAD")
	if got := git.RootCommit(t.Context(), dir); got != first {
		t.Fatalf("root %q, want the first commit %q", got, first)
	}

	write(t, dir, "more.txt", "more\n")
	commit(t, dir, "second")
	gitCmd(t, dir, "remote", "add", "origin", "git@github.com:someone/renamed.git")
	if got := git.RootCommit(t.Context(), dir); got != first {
		t.Fatalf("after a commit and a remote: %q, want %q", got, first)
	}

	empty := filepath.Join(t.TempDir(), "empty")
	initRepo(t, empty)
	if got := git.RootCommit(t.Context(), empty); got != "" {
		t.Fatalf("a repository with no commits: %q, want none", got)
	}

	shallow := filepath.Join(t.TempDir(), "shallow")
	if out, err := exec.Command("git", "clone", "--depth", "1", "file://"+dir, shallow).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v: %s", err, out)
	}
	if got := git.RootCommit(t.Context(), shallow); got != "" {
		t.Fatalf("a shallow clone: %q, want none", got)
	}

	if got := git.RootCommit(t.Context(), t.TempDir()); got != "" {
		t.Fatalf("a plain folder: %q, want none", got)
	}
}

func revParse(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", ref).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
