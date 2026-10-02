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

	"github.com/repogo/host/internal/errkind"
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

	history, err := service.Diff(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if history.Working.FilesChanged != 2 {
		t.Errorf("files changed = %d, want 2 (one edited, one new)",
			history.Working.FilesChanged)
	}
	// One added line in the tracked file, two in the new one.
	if history.Working.Additions != 3 {
		t.Errorf("additions = %d, want 3", history.Working.Additions)
	}
}

// The patch path: a file list, then one file's hunks, through the same method.
func TestPatchExpandsFilesThenHunks(t *testing.T) {
	dir, service := repo(t)
	ctx := context.Background()
	write(t, dir, "kept.txt", "one\ntwo\nthree\nfour\n")

	files, err := service.Patch(ctx, dir, "HEAD", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(files.Files) != 1 || files.Files[0].Path != "kept.txt" {
		t.Fatalf("files = %+v, want kept.txt", files.Files)
	}

	hunks, err := service.Patch(ctx, dir, "HEAD", "", "kept.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hunks.Text, "+four") {
		t.Errorf("patch does not contain the added line:\n%s", hunks.Text)
	}
}

// A moved file is listed at the path it now has. Without -z git writes a
// rename as `{old => new}/name`, and the client made a folder of the braces.
func TestPatchListsARenameAtItsNewPath(t *testing.T) {
	dir, service := repo(t)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(dir, "cmd", "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(1)\n\tfmt.Println(2)\n\tfmt.Println(3)\n}\n"
	write(t, dir, "cmd/old/main.go", body)
	commit(t, dir, "add")
	base := strings.TrimSpace(gitOut(t, dir, "rev-parse", "HEAD"))
	if err := os.Rename(filepath.Join(dir, "cmd", "old"), filepath.Join(dir, "cmd", "new")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "cmd/new/main.go", strings.Replace(body, "(3)", "(4)", 1))
	commit(t, dir, "move")

	files, err := service.Patch(ctx, dir, base, "HEAD", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(files.Files) != 1 {
		t.Fatalf("files = %+v, want the one moved file", files.Files)
	}
	got := files.Files[0]
	if got.Path != "cmd/new/main.go" || got.Added != 1 || got.Removed != 1 {
		t.Errorf("file = %+v, want cmd/new/main.go +1 -1", got)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// A revision starting with a dash would reach git as an option: `--output=`
// writes the diff to any file the host user can write.
func TestPatchRefusesARevisionThatIsAnOption(t *testing.T) {
	dir, service := repo(t)
	ctx := context.Background()
	target := filepath.Join(t.TempDir(), "overwritten")

	for _, args := range [][2]string{{"--output=" + target, ""}, {"HEAD", "--output=" + target}} {
		_, err := service.Patch(ctx, dir, args[0], args[1], "")
		if !errors.Is(err, ErrInvalidMutation) || errkind.Of(err) != errkind.Invalid {
			t.Errorf("Patch(%q, %q) = %v, want invalid", args[0], args[1], err)
		}
	}
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("git wrote %s: %v", target, err)
	}
}
