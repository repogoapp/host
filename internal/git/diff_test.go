package git

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/errkind"
)

// contained confines to one directory and resolves symlinks, as the real one
// does: on macOS a temp dir lives under the /var symlink.
type contained struct{ dir string }

func (c contained) Contain(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if resolved != c.dir && !strings.HasPrefix(resolved, c.dir+string(filepath.Separator)) {
		return "", fmt.Errorf("outside the root: %s", resolved)
	}
	return resolved, nil
}

// repo builds a real git repository with one commit: every number here comes
// out of git.
func repo(t *testing.T) (dir string, service *Service) {
	t.Helper()
	dir = t.TempDir()
	// macOS temp dirs are under a symlink (/var -> /private/var) and the
	// containment resolves paths, so the fixture has to resolve too or every
	// contained path fails to match.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	dir = resolved

	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write(t, dir, "kept.txt", "one\ntwo\nthree\n")
	commit(t, dir, "first")

	return dir, New(contained{dir}, slog.New(slog.DiscardHandler))
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, dir, message string) {
	t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", message}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
}

// An edit to a tracked file and a brand new file both have to be counted. The
// new file is the case `git diff` cannot see at all, and the one a coding agent
// produces most.
func TestWorkingSetCountsTrackedAndUntracked(t *testing.T) {
	dir, service := repo(t)
	write(t, dir, "kept.txt", "one\ntwo\nthree\nfour\n")
	write(t, dir, "new.txt", "alpha\nbeta\n")

	working, err := service.Diff(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if working.FilesChanged != 2 {
		t.Errorf("files changed = %d, want 2 (one edited, one new)",
			working.FilesChanged)
	}
	// One added line in the tracked file, two in the new one.
	if working.Additions != 3 {
		t.Errorf("additions = %d, want 3", working.Additions)
	}
}

// One file's hunks; the list is git.changes, so a call without a file is refused.
func TestPatchIsOneFilesHunks(t *testing.T) {
	dir, service := repo(t)
	ctx := context.Background()
	write(t, dir, "kept.txt", "one\ntwo\nthree\nfour\n")

	hunks, err := service.Patch(ctx, dir, "HEAD", "", "kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hunks.Text, "+four") {
		t.Errorf("patch does not contain the added line:\n%s", hunks.Text)
	}
	if _, err := service.Patch(ctx, dir, "HEAD", "", ""); !errors.Is(err, ErrInvalidMutation) {
		t.Errorf("Patch without a file = %v, want invalid", err)
	}
}

// A deleted file is listed with git's D, so the review counts and marks it as
// deleted, and its patch is the removal of every line.
func TestChangesMarkADeletedFileAndPatchRemovesIt(t *testing.T) {
	dir, service := repo(t)
	ctx := context.Background()
	if err := os.Remove(filepath.Join(dir, "kept.txt")); err != nil {
		t.Fatal(err)
	}

	changes, err := service.Changes(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "kept.txt" || changes[0].Code != " D" || changes[0].Deletions != 3 {
		t.Fatalf("changes = %+v, want kept.txt \" D\" -3", changes)
	}

	patch, err := service.Patch(ctx, dir, "HEAD", "", "kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(patch.Text, "deleted file mode") || !strings.Contains(patch.Text, "-three") {
		t.Errorf("patch is not a deletion:\n%s", patch.Text)
	}
}

// A new folder's files are listed one by one with their lines, as the header's
// totals count them; status alone would name only the folder.
func TestChangesListsEachUntrackedFileWithItsLines(t *testing.T) {
	dir, service := repo(t)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(dir, "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "web/index.html", "<p>\n</p>\n")
	write(t, dir, "web/app.js", "go()\n")

	changes, err := service.Changes(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]FileChange{}
	for _, change := range changes {
		byPath[change.Path] = change
	}
	if len(changes) != 2 || byPath["web/index.html"].Code != "??" || byPath["web/index.html"].Additions != 2 ||
		byPath["web/app.js"].Additions != 1 {
		t.Errorf("changes = %+v, want both files ?? with their lines", changes)
	}

	working, err := service.Diff(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if working.FilesChanged != len(changes) {
		t.Errorf("totals count %d files, the list %d", working.FilesChanged, len(changes))
	}
}

// A revision starting with a dash would reach git as an option: `--output=`
// writes the diff to any file the host user can write.
func TestPatchRefusesARevisionThatIsAnOption(t *testing.T) {
	dir, service := repo(t)
	ctx := context.Background()
	target := filepath.Join(t.TempDir(), "overwritten")

	for _, args := range [][2]string{{"--output=" + target, ""}, {"HEAD", "--output=" + target}} {
		_, err := service.Patch(ctx, dir, args[0], args[1], "kept.txt")
		if !errors.Is(err, ErrInvalidMutation) || errkind.Of(err) != errkind.Invalid {
			t.Errorf("Patch(%q, %q) = %v, want invalid", args[0], args[1], err)
		}
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("git wrote %s: %v", target, err)
	}
}
