package github

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogo/host/internal/git"
)

// fakeCreate records its arguments and prints the URL `gh repo create` prints.
const fakeCreate = `#!/bin/sh
echo "$@" > "$FAKE_GH/args"
echo "https://github.com/octocat/$3"
`

func withFakeCreate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(fakeCreate), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GH", dir)
	return dir
}

// A plain folder becomes a repository on main, and gh is asked to create the
// named repo from it as origin.
func TestPublishInitialisesAFolderAndCreatesTheRepo(t *testing.T) {
	gh := withFakeCreate(t)
	project := t.TempDir()

	got, err := newService(t, t.TempDir()).Publish(context.Background(), project, "newwww", true)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	want := Published{NameWithOwner: "octocat/newwww", URL: "https://github.com/octocat/newwww", Branch: "main"}
	if got != want {
		t.Fatalf("Publish = %+v, want %+v", got, want)
	}
	if _, err := os.Stat(filepath.Join(project, ".git")); err != nil {
		t.Fatalf("folder was not initialised: %v", err)
	}
	args, _ := os.ReadFile(filepath.Join(gh, "args"))
	if want := "repo create newwww --private --source " + project + " --remote origin"; strings.TrimSpace(string(args)) != want {
		t.Fatalf("gh args = %q, want %q", args, want)
	}
}

func TestPublishPublic(t *testing.T) {
	gh := withFakeCreate(t)
	if _, err := newService(t, t.TempDir()).Publish(context.Background(), t.TempDir(), "site", false); err != nil {
		t.Fatal(err)
	}
	if args, _ := os.ReadFile(filepath.Join(gh, "args")); !strings.Contains(string(args), "--public") {
		t.Fatalf("gh args = %q, want --public", args)
	}
}

// Anything but a bare name is refused before git or gh runs.
func TestPublishRefusesBadNames(t *testing.T) {
	gh := withFakeCreate(t)
	project := t.TempDir()
	for _, name := range []string{"", ".", "..", "owner/name", "-flag", "na me", "na\nme", strings.Repeat("a", 101)} {
		if _, err := newService(t, t.TempDir()).Publish(context.Background(), project, name, true); !errors.Is(err, ErrBadRepoName) {
			t.Errorf("Publish(%q) = %v, want ErrBadRepoName", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(project, ".git")); err == nil {
		t.Fatal("a refused name initialised the folder")
	}
	if _, err := os.Stat(filepath.Join(gh, "args")); err == nil {
		t.Fatal("a refused name reached gh")
	}
}

// A project that already has an origin is never given a second repository.
func TestPublishRefusesAProjectWithAnOrigin(t *testing.T) {
	gh := withFakeCreate(t)
	project := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"remote", "add", "origin", "https://github.com/octocat/old.git"}} {
		if _, err := git.Run(context.Background(), project, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := newService(t, t.TempDir()).Publish(context.Background(), project, "new", true); !errors.Is(err, git.ErrHasOrigin) {
		t.Fatalf("Publish = %v, want ErrHasOrigin", err)
	}
	if _, err := os.Stat(filepath.Join(gh, "args")); err == nil {
		t.Fatal("gh ran for a project with an origin")
	}
}

func TestPublishedReadsTheLastLine(t *testing.T) {
	got, err := published("✓ Created repository octocat/x on GitHub\nhttps://github.com/octocat/x\n", "main")
	if err != nil || got.NameWithOwner != "octocat/x" {
		t.Fatalf("published = %+v, %v", got, err)
	}
	for _, out := range []string{"", "done", "https://github.com/octocat"} {
		if _, err := published(out, "main"); err == nil {
			t.Errorf("published(%q) succeeded", out)
		}
	}
}
