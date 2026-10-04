package projectsync

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/github"
	"github.com/repogo/host/internal/store"
)

// git's own invariant does the first cut, and the owned directory the second.
func TestKindOf(t *testing.T) {
	root := t.TempDir()
	checkouts := filepath.Join(root, "checkouts")

	// A clone: .git is a directory.
	clone := filepath.Join(root, "clone")
	mkdir(t, filepath.Join(clone, ".git"))

	// A linked worktree we made: .git is a FILE, inside the owned directory.
	managed := filepath.Join(checkouts, "octocat", "code", "main-a91c4f")
	mkdir(t, managed)
	write(t, filepath.Join(managed, ".git"), "gitdir: /elsewhere\n")

	// A linked worktree somebody made by hand, outside it.
	loose := filepath.Join(root, "hand-made")
	mkdir(t, loose)
	write(t, filepath.Join(loose, ".git"), "gitdir: /elsewhere\n")

	// No git at all — an ordinary folder, which is a supported project.
	plain := filepath.Join(root, "notes")
	mkdir(t, plain)

	for _, c := range []struct {
		path string
		want store.ProjectKind
	}{
		{clone, store.ProjectClone},
		{managed, store.ProjectManaged},
		{loose, store.ProjectWorktree},
		{plain, store.ProjectFolder},
	} {
		if got := kindOf(c.path, checkouts); got != c.want {
			t.Errorf("kindOf(%s) = %q, want %q", filepath.Base(c.path), got, c.want)
		}
	}

	// A host that creates no worktrees can have nothing managed.
	if got := kindOf(managed, ""); got != store.ProjectWorktree {
		t.Errorf("with no checkouts dir, kindOf = %q, want %q", got, store.ProjectWorktree)
	}
}

// The clone layout names the repository for free; everything else has to ask
// git, and a folder with no origin has no repository at all.
func TestRepoFromLayout(t *testing.T) {
	checkouts := t.TempDir()
	path := filepath.Join(checkouts, "Octocat", "Code", "main-a91c4f")

	// Case kept: the phone shows the name as spelled and folds case itself
	// when it groups `Octocat/Code` with `octocat/code`.
	owner, name, ok := github.CheckoutRepo(checkouts, path)
	if !ok || owner != "Octocat" || name != "Code" {
		t.Errorf("CheckoutRepo = %q/%q ok=%v, want Octocat/Code", owner, name, ok)
	}

	// The clone directory itself is two levels deep, not three — not a checkout.
	if _, _, ok := github.CheckoutRepo(checkouts, filepath.Join(checkouts, "octocat")); ok {
		t.Error("a bare owner directory was read as a checkout")
	}
	if _, _, ok := github.CheckoutRepo(checkouts, "/somewhere/else"); ok {
		t.Error("a path outside the checkouts dir was read as a checkout")
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type listed []string

func (l listed) Projects() ([]files.Entry, error) {
	out := make([]files.Entry, len(l))
	for i, path := range l {
		out[i] = files.Entry{Path: path}
	}
	return out, nil
}

type recorded struct{ rows []store.Project }

func (*recorded) Activity() (map[string]store.Activity, error) { return nil, nil }
func (*recorded) KnownRepos() (map[string]bool, error)         { return nil, nil }
func (r *recorded) SyncProjects(rows []store.Project) (store.ProjectChange, error) {
	r.rows = rows
	return store.ProjectChange{Changed: rows}, nil
}

// ignore is a changed hook that hears nothing.
func ignore(store.ProjectChange) {}

// noIcons is a project service whose projects have no icon.
type noIcons struct{}

func (noIcons) IconHash(string) string { return "" }

// A plain folder names no repository, even inside a checkout whose origin git
// would otherwise report for it.
func TestAPlainFolderInsideARepoHasNoRepository(t *testing.T) {
	parent := t.TempDir()
	for _, args := range [][]string{
		{"init", parent},
		{"-C", parent, "remote", "add", "origin", "git@github.com:octocat/parent.git"},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	notes := filepath.Join(parent, "notes")
	mkdir(t, notes)

	db := &recorded{}
	New(listed{notes}, db, noIcons{}, "", ignore, slog.New(slog.DiscardHandler)).Once(context.Background())
	if len(db.rows) != 1 || db.rows[0].Kind != store.ProjectFolder || db.rows[0].RepoOwner != "" || db.rows[0].RepoName != "" {
		t.Errorf("rows = %+v, want one folder with no repository", db.rows)
	}
}
