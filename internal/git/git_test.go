package git_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/files"
	"github.com/repogoapp/host/internal/git"
)

// Real repositories, real `git`. A fake would only prove this package agrees
// with my idea of porcelain output, which is the thing most likely to be wrong.

func repo(t *testing.T) (svc *git.Service, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir = filepath.Join(base, "project")
	initRepo(t, dir)
	write(t, dir, "README.md", "hello\n")
	commit(t, dir, "first")

	return git.New(files.New(files.Config{Roots: files.StaticRoots{dir}}), slog.New(slog.DiscardHandler)), dir
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, dir, message string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-m", message}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

func TestStatusReportsBranchAndCleanTree(t *testing.T) {
	svc, dir := repo(t)

	got, err := svc.Status(context.Background(), []string{dir})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	st := got[0]
	if !st.Repo {
		t.Fatal("a git repository was not recognized as one")
	}
	if st.Branch != "main" {
		t.Errorf("branch = %q, want main", st.Branch)
	}
	if dirty := st.Staged + st.Unstaged + st.Untracked; dirty != 0 {
		t.Errorf("a fresh commit left %d dirty entries", dirty)
	}
	// No remote, so nothing to be ahead or behind of. Reported as zero rather
	// than as an error, because a branch that was never pushed is normal.
	if st.Ahead != 0 || st.Behind != 0 {
		t.Errorf("ahead/behind = %d/%d without an upstream", st.Ahead, st.Behind)
	}
}

// A file can be staged AND modified again since. Git shows that as two non-space
// columns, and counting it once would under-report what is uncommitted.
func TestStatusCountsStagedUnstagedAndUntrackedSeparately(t *testing.T) {
	svc, dir := repo(t)

	write(t, dir, "staged.txt", "a\n")
	run(t, dir, "add", "staged.txt")

	write(t, dir, "README.md", "changed\n") // tracked, modified, not staged
	write(t, dir, "new.txt", "b\n")         // untracked

	got, _ := svc.Status(context.Background(), []string{dir})
	st := got[0]
	if st.Staged != 1 {
		t.Errorf("staged = %d, want 1", st.Staged)
	}
	if st.Unstaged != 1 {
		t.Errorf("unstaged = %d, want 1", st.Unstaged)
	}
	if st.Untracked != 1 {
		t.Errorf("untracked = %d, want 1", st.Untracked)
	}
}

func TestStatusCountsAStagedRenameOnce(t *testing.T) {
	svc, dir := repo(t)

	write(t, dir, "old name.txt", "one\n")
	commit(t, dir, "add")
	run(t, dir, "mv", "old name.txt", "new name.txt")

	got, _ := svc.Status(context.Background(), []string{dir})
	if st := got[0]; st.Staged != 1 || st.Unstaged != 0 || st.Untracked != 0 {
		t.Errorf("status = %+v, want one staged file", st)
	}
}

// "Open a folder and talk to an agent" works before anyone runs `git init`, so a
// plain directory is a plain answer and not an error.
func TestPlainFolderIsNotAnError(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := git.New(files.New(files.Config{Roots: files.StaticRoots{base}}), slog.New(slog.DiscardHandler))

	got, err := svc.Status(context.Background(), []string{base})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if got[0].Repo {
		t.Error("a plain folder was reported as a repository")
	}
	if got[0].Error != "" {
		t.Errorf("a plain folder reported an error: %s", got[0].Error)
	}
}

// One deleted project must not blank the whole sidebar.
func TestOneBadPathDoesNotFailTheBatch(t *testing.T) {
	svc, dir := repo(t)

	got, err := svc.Status(context.Background(), []string{dir, "/etc"})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !got[0].Repo {
		t.Error("the good project was lost")
	}
	if got[1].Error == "" {
		t.Error("a path outside every project reported no error")
	}
	// Results stay positional: a client renders them beside what it asked for.
	if got[1].Path != "/etc" {
		t.Errorf("results were reordered: %q", got[1].Path)
	}
}

func TestStatusRefusesAnOversizedBatch(t *testing.T) {
	svc, dir := repo(t)

	paths := make([]string, git.MaxBatch+1)
	for i := range paths {
		paths[i] = dir
	}
	if _, err := svc.Status(context.Background(), paths); !errors.Is(err, git.ErrTooManyPaths) {
		t.Errorf("oversized batch: %v, want ErrTooManyPaths", err)
	}
}

func TestChangesCountsLinesForStagedAndUnstagedAlike(t *testing.T) {
	svc, dir := repo(t)

	write(t, dir, "README.md", "hello\nsecond\nthird\n")
	write(t, dir, "added.txt", "x\n")
	run(t, dir, "add", "added.txt")

	changes, err := svc.Changes(context.Background(), dir)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	byPath := map[string]int{}
	for _, c := range changes {
		byPath[c.Path] = c.Additions
	}
	if byPath["README.md"] != 2 {
		t.Errorf("README.md added = %d, want 2", byPath["README.md"])
	}
	// Diffed against HEAD, not the index: a staged file reporting zero changed
	// lines reads as "nothing happened".
	if byPath["added.txt"] != 1 {
		t.Errorf("staged file added = %d, want 1", byPath["added.txt"])
	}
}

// A staged rename keeps its line counts: status names the new path, and the
// numstat has to name the same one.
func TestChangesCountsLinesForAStagedRename(t *testing.T) {
	svc, dir := repo(t)

	write(t, dir, "old.txt", "one\ntwo\nthree\nfour\nfive\n")
	commit(t, dir, "add")
	run(t, dir, "mv", "old.txt", "new.txt")
	write(t, dir, "new.txt", "one\ntwo\nthree\nfour\nfive\nsix\n")
	run(t, dir, "add", "new.txt")

	changes, err := svc.Changes(context.Background(), dir)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if len(changes) != 1 || changes[0].Path != "new.txt" || changes[0].Additions != 1 {
		t.Errorf("changes = %+v, want new.txt +1", changes)
	}
}

// Without -z git quotes a path with a space, a quote or non-ASCII, and the
// quoted spelling misses its numstat line, so the file shows zero lines.
func TestChangesCountsLinesForAQuotedPath(t *testing.T) {
	svc, dir := repo(t)

	names := []string{`with space.txt`, `quo"te.txt`, "café.txt"}
	for _, name := range names {
		write(t, dir, name, "one\n")
	}
	commit(t, dir, "add")
	for _, name := range names {
		write(t, dir, name, "one\ntwo\nthree\n")
	}

	changes, err := svc.Changes(context.Background(), dir)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	byPath := map[string]git.FileChange{}
	for _, c := range changes {
		byPath[c.Path] = c
	}
	for _, name := range names {
		if got := byPath[name]; got.Code != " M" || got.Additions != 2 {
			t.Errorf("%q = %+v, want \" M\" +2 (all: %+v)", name, got, changes)
		}
	}
}

func TestChangesRefusesAPathOutsideEveryProject(t *testing.T) {
	svc, _ := repo(t)

	if _, err := svc.Changes(context.Background(), "/etc"); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("changes outside a root: %v, want ErrOutsideRoots", err)
	}
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// git is run for its side effects on a fixture; a failure here is the fixture,
// not the code under test.
func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

func TestBranchesListsLocalAndRemoteNewestFirst(t *testing.T) {
	svc, dir := repo(t)

	gitCmd(t, dir, "branch", "old-work")
	gitCmd(t, dir, "checkout", "-b", "fix-the-drawer")
	write(t, dir, "drawer.go", "package main\n")
	commit(t, dir, "second")

	// A remote without a network: `origin/…` is just a ref, and pointing one at
	// a commit is what a fetch would have left behind.
	gitCmd(t, dir, "update-ref", "refs/remotes/origin/only-on-the-remote", "main")

	branches, err := svc.Branches(context.Background(), dir)
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}

	byName := map[string]git.Branch{}
	for _, branch := range branches {
		byName[branch.Name] = branch
	}
	for _, name := range []string{"main", "old-work", "fix-the-drawer", "origin/only-on-the-remote"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("Branches is missing %q: got %v", name, branches)
		}
	}
	if !byName["fix-the-drawer"].Current {
		t.Error("fix-the-drawer is the checked out branch, want Current")
	}
	if byName["main"].Current {
		t.Error("main is not checked out, want Current false")
	}
	if !byName["origin/only-on-the-remote"].Remote {
		t.Error("origin/only-on-the-remote should be Remote")
	}
	if byName["main"].Remote {
		t.Error("main is local, want Remote false")
	}
	if byName["fix-the-drawer"].SHA == "" {
		t.Errorf("fix-the-drawer = %+v, want a sha", byName["fix-the-drawer"])
	}

	// The current branch is hoisted, and the rest stay in commit order — the
	// second commit is on fix-the-drawer, so main and old-work follow it.
	if branches[0].Name != "fix-the-drawer" {
		t.Errorf("Branches[0] = %q, want the checked out branch first", branches[0].Name)
	}
}

// The default branch is the one thing a picker has to know before it opens, and
// the repository states it as `origin/HEAD` rather than by convention.
func TestBranchesMarksTheDefaultAndFoldsRemoteDuplicates(t *testing.T) {
	svc, dir := repo(t)

	gitCmd(t, dir, "checkout", "-b", "side")
	gitCmd(t, dir, "update-ref", "refs/remotes/origin/main", "main")
	gitCmd(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	branches, err := svc.Branches(context.Background(), dir)
	if err != nil {
		t.Fatalf("Branches: %v", err)
	}

	if branches[0].Name != "main" || !branches[0].Default {
		t.Errorf("Branches[0] = %+v, want main marked as the default and first", branches[0])
	}
	for _, branch := range branches {
		if branch.Name == "origin/main" {
			t.Error("origin/main duplicates the local main and should be folded away")
		}
		if branch.Name == "origin/HEAD" {
			t.Error("origin/HEAD is a symbolic ref, not a branch")
		}
	}
	if len(branches) != 2 {
		t.Errorf("Branches = %v, want main and side", branches)
	}
}

func TestBranchesRefusesAPathOutsideEveryProject(t *testing.T) {
	svc, _ := repo(t)
	if _, err := svc.Branches(context.Background(), "/etc"); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("Branches(/etc) = %v, want ErrOutsideRoots", err)
	}
}

// A submodule and a nested checkout are each one row marked repo, slash-free,
// so the phone can open them as projects; a plain or ignored folder is not.
func TestChangesMarksChildRepos(t *testing.T) {
	svc, dir := repo(t)

	upstream := filepath.Join(filepath.Dir(dir), "upstream")
	initRepo(t, upstream)
	write(t, upstream, "lib.go", "package lib\n")
	commit(t, upstream, "lib")
	run(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", upstream, "sub")
	commit(t, dir, "submodule")
	write(t, filepath.Join(dir, "sub"), "lib.go", "package lib\n\nfunc A() {}\n")

	initRepo(t, filepath.Join(dir, "nested"))
	write(t, filepath.Join(dir, "nested"), "a.txt", "a\n")
	if err := os.MkdirAll(filepath.Join(dir, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "plain"), "b.txt", "b\n")
	write(t, dir, ".gitignore", "ignored/\n")
	initRepo(t, filepath.Join(dir, "ignored"))
	write(t, filepath.Join(dir, "ignored"), "c.txt", "c\n")

	changes, err := svc.Changes(context.Background(), dir)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	byPath := map[string]git.FileChange{}
	for _, c := range changes {
		byPath[c.Path] = c
	}
	for _, name := range []string{"sub", "nested"} {
		if !byPath[name].Repo {
			t.Errorf("%s = %+v, want repo (all: %+v)", name, byPath[name], changes)
		}
	}
	for _, name := range []string{"plain/b.txt", ".gitignore"} {
		if got, ok := byPath[name]; !ok || got.Repo {
			t.Errorf("%s = %+v, want a plain row (all: %+v)", name, got, changes)
		}
	}
	for _, name := range []string{"nested/", "ignored", "ignored/"} {
		if _, ok := byPath[name]; ok {
			t.Errorf("%s listed (all: %+v)", name, changes)
		}
	}
}

func initRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"config", "commit.gpgsign", "false"},
	} {
		run(t, dir, args...)
	}
}

// A project inside a larger checkout lists, totals and diffs its own folder,
// every path relative to it, whether git tracks the file or not.
func TestASubfolderProjectReadsItsOwnFolder(t *testing.T) {
	_, root := repo(t)
	sub := filepath.Join(root, "app")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, sub, "tracked.txt", "one\n")
	commit(t, root, "app")
	write(t, sub, "tracked.txt", "one\ntwo\n")
	write(t, sub, "new.txt", "new\n")
	write(t, root, "outside.txt", "not the project's\n")
	svc := git.New(files.New(files.Config{Roots: files.StaticRoots{sub}}), slog.New(slog.DiscardHandler))
	ctx := context.Background()

	changes, err := svc.Changes(ctx, sub)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	paths := map[string]int{}
	for _, c := range changes {
		paths[c.Path] = c.Additions
	}
	if len(paths) != 2 || paths["tracked.txt"] != 1 || paths["new.txt"] != 1 {
		t.Errorf("changes = %+v, want tracked.txt +1 and new.txt +1", changes)
	}
	if set, err := svc.Diff(ctx, sub); err != nil || set == nil || set.FilesChanged != 2 || set.Additions != 2 {
		t.Errorf("diff = %+v, %v, want 2 files +2", set, err)
	}
	patch, err := svc.Patch(ctx, sub, "HEAD", "", "tracked.txt")
	if err != nil || !strings.Contains(patch.Text, "+two") {
		t.Errorf("patch = %q, %v, want +two", patch.Text, err)
	}
}

// Before the first commit a staged file still lists and still diffs.
func TestARepoWithNoCommitsDiffsAgainstTheEmptyTree(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "fresh")
	initRepo(t, dir)
	write(t, dir, "a.txt", "a\n")
	run(t, dir, "add", "a.txt")
	svc := git.New(files.New(files.Config{Roots: files.StaticRoots{dir}}), slog.New(slog.DiscardHandler))

	patch, err := svc.Patch(context.Background(), dir, "HEAD", "", "a.txt")
	if err != nil || !strings.Contains(patch.Text, "+a") {
		t.Errorf("patch = %q, %v, want +a", patch.Text, err)
	}
}
