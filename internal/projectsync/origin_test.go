package projectsync

import (
	"context"
	"log/slog"
	"os/exec"
	"testing"

	"github.com/repogoapp/host/internal/store"
)

// A pass records each checkout's first commit beside its repository.
func TestAPassRecordsTheRootCommit(t *testing.T) {
	dir := gitRepo(t, "git@github.com:octocat/hello.git")
	db := &recorded{}
	New(listed{dir}, db, noIcons{}, "", ignore, slog.New(slog.DiscardHandler)).Once(context.Background())
	if len(db.rows) != 1 || db.rows[0].RootCommit == "" || db.rows[0].RepoName != "hello" {
		t.Fatalf("rows = %+v, want one checkout with its repository and root", db.rows)
	}
}

// A new origin reaches devices once; reading the same origin again, or one
// that is gone, tells nobody.
func TestRefreshOriginAnnouncesOnlyAMove(t *testing.T) {
	dir := gitRepo(t, "git@github.com:octocat/hello.git")
	db := &recorded{repo: "octocat/hello"}
	var heard []store.ProjectChange
	s := New(listed{dir}, db, noIcons{}, "", func(c store.ProjectChange) { heard = append(heard, c) },
		slog.New(slog.DiscardHandler))

	s.RefreshOrigin(context.Background(), dir)
	if len(heard) != 0 {
		t.Fatalf("an unchanged origin was announced: %+v", heard)
	}

	gitIn(t, dir, "remote", "set-url", "origin", "https://github.com/new-owner/renamed.git")
	s.RefreshOrigin(context.Background(), dir)
	if len(heard) != 1 || heard[0].Changed[0].RepoOwner != "new-owner" || heard[0].Changed[0].RepoName != "renamed" {
		t.Fatalf("after set-url: %+v, want one change to new-owner/renamed", heard)
	}

	gitIn(t, dir, "remote", "remove", "origin")
	s.RefreshOrigin(context.Background(), dir)
	if len(heard) != 1 || db.repo != "new-owner/renamed" {
		t.Fatalf("a removed origin changed the name: %+v, %q", heard, db.repo)
	}
}

// gitRepo is a checkout with one commit and `origin` set to remote.
func gitRepo(t *testing.T, remote string) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "--initial-branch=main")
	gitIn(t, dir, "-c", "user.email=t@example.com", "-c", "user.name=T", "-c", "commit.gpgsign=false",
		"commit", "--allow-empty", "-m", "first")
	gitIn(t, dir, "remote", "add", "origin", remote)
	return dir
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}
