package github

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/git"
)

// The name is the only untrusted input that becomes a path, so it gets the
// attention. Everything rejected here is rejected before anything is created
// and before gh is given the string as an argument.
func TestCloneRefusesAnythingButOwnerName(t *testing.T) {
	dir := t.TempDir()
	s := newService(t, dir)

	bad := []string{
		"",
		"repo",                   // no owner
		"owner/name/extra",       // not a repo name
		"../../../etc",           // traversal
		"owner/../../../etc",     // traversal past the clone dir
		"owner/..",               // the clone dir itself
		"-flag/name",             // would read as a gh flag
		"owner/-flag",            // same, on the other half
		"owner/name;rm -rf /",    // shell metacharacters
		"owner/na me",            // whitespace
		"owner/na\nme",           // newline
		"https://github.com/o/n", // a URL, not a name
	}
	for _, name := range bad {
		if _, err := s.Clone(context.Background(), name); !errors.Is(err, ErrBadName) {
			t.Errorf("Clone(%q) = %v, want ErrBadName", name, err)
		}
	}

	// Nothing was created on the way to any of those refusals.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("refused clones left %d entries behind", len(entries))
	}
}

func TestRootsAreTheClonedFolders(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"alpha", "beta", ".git-internal"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A stray file is not a project.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	roots, err := newService(t, dir).Roots()
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	want := []string{filepath.Join(dir, "alpha"), filepath.Join(dir, "beta")}
	if len(roots) != len(want) {
		t.Fatalf("Roots = %v, want %v", roots, want)
	}
	for i := range want {
		if roots[i] != want[i] {
			t.Errorf("Roots[%d] = %q, want %q", i, roots[i], want[i])
		}
	}
}

// A clone is owner-scoped, so two accounts with a repository of the same name
// are two checkouts. Flat naming made the second one silently open the first.
func TestRootsAreOwnerScoped(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"octocat/code/main", "alice/code/main"} {
		if err := os.MkdirAll(filepath.Join(dir, ".worktrees", name, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A loose folder in the clone directory is still a project, and the owner
	// directory holding the checkouts is not one.
	if err := os.MkdirAll(filepath.Join(dir, "by-hand"), 0o755); err != nil {
		t.Fatal(err)
	}

	roots, err := newService(t, dir).Roots()
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	want := []string{
		filepath.Join(dir, "by-hand"),
		filepath.Join(dir, ".worktrees", "alice", "code", "main"),
		filepath.Join(dir, ".worktrees", "octocat", "code", "main"),
	}
	if len(roots) != len(want) {
		t.Fatalf("Roots = %v, want %v", roots, want)
	}
	for i := range want {
		if roots[i] != want[i] {
			t.Errorf("Roots[%d] = %q, want %q", i, roots[i], want[i])
		}
	}
}

// The clone directory is created on first clone, so a host nobody has cloned on
// has no roots rather than an error — which would otherwise take fs.* down with
// it on every fresh install.
func TestRootsBeforeAnyoneHasCloned(t *testing.T) {
	roots, err := newService(t, filepath.Join(t.TempDir(), "never-created")).Roots()
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	if len(roots) != 0 {
		t.Errorf("Roots = %v, want none", roots)
	}
}

// A repository's chat worktrees are projects beside its clone; a folder in
// the worktree tree that is not a checkout is a half-finished one.
func TestWorktreesBesideTheCloneAreProjects(t *testing.T) {
	dir := t.TempDir()
	clone := filepath.Join(dir, ".repos", "octocat", "code")
	if err := os.MkdirAll(filepath.Join(clone, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	checkouts := CheckoutParent(NewLayout(dir).Checkouts(), "octocat", "code")
	for _, name := range []string{"main-a91c4f", "main-3f0e21"} {
		if err := os.MkdirAll(filepath.Join(checkouts, name), 0o755); err != nil {
			t.Fatal(err)
		}
		link := []byte("gitdir: " + filepath.Join(clone, ".git", "worktrees", name))
		if err := os.WriteFile(filepath.Join(checkouts, name, ".git"), link, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(checkouts, "died-mid-create"), 0o755); err != nil {
		t.Fatal(err)
	}

	roots, err := NewLayout(dir).Roots()
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	want := []string{
		clone,
		filepath.Join(checkouts, "main-3f0e21"),
		filepath.Join(checkouts, "main-a91c4f"),
	}
	if !slices.Equal(roots, want) {
		t.Fatalf("Roots = %v, want %v", roots, want)
	}
	if owner, repo, ok := CheckoutRepo(NewLayout(dir).Checkouts(), want[1]); !ok || owner != "octocat" || repo != "code" {
		t.Errorf("CheckoutRepo = %q %q %v, want octocat code", owner, repo, ok)
	}
}

// fakeClone "clones" by making the destination a repository, as far as the
// layout can tell: `gh repo clone <owner/name> <dest>`.
const fakeClone = `#!/bin/sh
mkdir -p "$4/.git"
`

// A clone is `.repos/<owner>/<repo>`, no branch in the path, so switching
// branch never makes the path stale. It is a project, listed as cloned, and a
// second clone opens it.
func TestCloneIsTheRepoFolder(t *testing.T) {
	gh := t.TempDir()
	if err := os.WriteFile(filepath.Join(gh, "gh"), []byte(fakeClone), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", gh+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	s := newService(t, dir)
	clone, err := s.Clone(context.Background(), "octocat/code")
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	want := filepath.Join(dir, ".repos", "octocat", "code")
	if clone.Path != want || clone.Existed {
		t.Fatalf("Clone = %+v, want a new clone at %q", clone, want)
	}
	// Nothing left behind from the download.
	if entries, _ := os.ReadDir(filepath.Dir(want)); len(entries) != 1 {
		t.Errorf("owner folder holds %d entries, want only the clone", len(entries))
	}

	roots, err := s.Roots()
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	if len(roots) != 1 || roots[0] != want {
		t.Errorf("Roots = %v, want [%q]", roots, want)
	}
	if got := clonedPath(s.clonedPaths(), "Octocat/Code"); got != want {
		t.Errorf("clonedPath = %q, want %q", got, want)
	}

	again, err := s.Clone(context.Background(), "octocat/code")
	if err != nil {
		t.Fatalf("second Clone: %v", err)
	}
	if !again.Existed || again.Path != want {
		t.Errorf("second Clone = %+v, want the existing clone", again)
	}
}

// A folder with anything in it is never cloned over or cleared: a second
// clone of a repository still downloading must not wipe the first.
func TestCloneRefusesAFolderThatIsNotEmpty(t *testing.T) {
	gh := t.TempDir()
	if err := os.WriteFile(filepath.Join(gh, "gh"), []byte(fakeClone), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", gh+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	dest := filepath.Join(dir, ".repos", "octocat", "code")
	partial := filepath.Join(dest, "half-downloaded")
	if err := os.MkdirAll(partial, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := newService(t, dir).Clone(context.Background(), "octocat/code"); !errors.Is(err, ErrFolderExists) {
		t.Fatalf("Clone = %v, want ErrFolderExists", err)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Errorf("the folder's contents were touched: %v", err)
	}

	// An empty folder, as a failed clone leaves, is reused.
	if err := os.RemoveAll(partial); err != nil {
		t.Fatal(err)
	}
	if clone, err := newService(t, dir).Clone(context.Background(), "octocat/code"); err != nil || clone.Path != dest {
		t.Fatalf("Clone = %+v, %v; want a clone at %q", clone, err, dest)
	}
}

// anyPath contains every path, for tests about gh rather than containment.
type anyPath struct{}

func (anyPath) Contain(path string) (string, error) { return path, nil }

func newService(t *testing.T, dir string) *Service {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	return New(NewLayout(dir), git.New(anyPath{}, log), log)
}

// Every path a client names is contained before gh or git runs in it, and a
// missing title is refused before anything is pushed.
func TestPathMethodsContainAndCreatePRNeedsATitle(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	s := New(NewLayout(t.TempDir()), git.New(files.New(files.Config{Roots: files.StaticRoots{t.TempDir()}}), log), log)
	ctx := context.Background()
	outside := t.TempDir()

	checks := map[string]error{}
	_, checks["pr_create"] = s.CreatePR(ctx, outside, NewPR{Title: "t"})
	_, checks["publish"] = s.Publish(ctx, outside, "name", true)
	for name, err := range checks {
		if !errors.Is(err, files.ErrOutsideRoots) {
			t.Errorf("%s outside every root = %v, want ErrOutsideRoots", name, err)
		}
	}
	if _, err := s.CreatePR(ctx, outside, NewPR{Title: "  "}); !errors.Is(err, ErrNoTitle) || errkind.Of(err) != errkind.Invalid {
		t.Errorf("CreatePR without a title = %v, want ErrNoTitle", err)
	}
}
