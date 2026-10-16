package projectsync

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/repogoapp/host/internal/files"
	"github.com/repogoapp/host/internal/store"
)

// A clone has `.git`, a directory or a linked checkout's file; anything
// else is a folder.
func TestKindOf(t *testing.T) {
	root := t.TempDir()

	clone := filepath.Join(root, "clone")
	mkdir(t, filepath.Join(clone, ".git"))

	linked := filepath.Join(root, "linked")
	mkdir(t, linked)
	write(t, filepath.Join(linked, ".git"), "gitdir: /elsewhere\n")

	// No git at all — an ordinary folder, which is a supported project.
	plain := filepath.Join(root, "notes")
	mkdir(t, plain)

	for _, c := range []struct {
		path string
		want store.ProjectKind
	}{
		{clone, store.ProjectClone},
		{linked, store.ProjectClone},
		{plain, store.ProjectFolder},
	} {
		if got := kindOf(c.path); got != c.want {
			t.Errorf("kindOf(%s) = %q, want %q", filepath.Base(c.path), got, c.want)
		}
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
func (*recorded) ProjectsAt([]string) ([]store.Project, error) { return nil, nil }
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
