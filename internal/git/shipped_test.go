package git_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogo/host/internal/git"
)

// withRemote gives repo a bare origin whose fetch URL names acme/widgets, so
// owner and name are checked without a network.
func withRemote(t *testing.T, dir string) {
	t.Helper()
	ctx := context.Background()
	bare := t.TempDir()
	if _, err := git.Run(ctx, bare, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"remote", "add", "origin", "https://github.com/acme/widgets.git"},
		{"remote", "set-url", "--push", "origin", bare},
	} {
		if _, err := git.Run(ctx, dir, args...); err != nil {
			t.Fatal(err)
		}
	}
}

func lines(n int) string {
	return strings.Repeat("x\n", n)
}

func shas(s *git.Shipped) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s.Commits))
	for i, c := range s.Commits {
		out[i] = c.SHA
	}
	return out
}

func head(t *testing.T, dir string) string {
	t.Helper()
	out, err := git.Run(context.Background(), dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func TestPushReportsOnlyWhatItShipped(t *testing.T) {
	svc, dir := repo(t)
	withRemote(t, dir)
	ctx := context.Background()

	// First push of main: a new branch on an empty remote ships everything.
	write(t, dir, "a.go", "one\n")
	first, err := svc.CommitPush(ctx, dir, "secret subject one")
	if err != nil || first.Shipped == nil {
		t.Fatalf("first push: %+v, %v", first, err)
	}
	if got := len(first.Shipped.Commits); got != 2 {
		t.Fatalf("first push shipped %d commits, want 2", got)
	}
	s := first.Shipped
	if s.RepoOwner != "acme" || s.RepoName != "widgets" || s.Branch != "main" || s.EventID == "" || s.PushedAtMs == 0 {
		t.Fatalf("envelope: %+v", s)
	}

	// An existing branch ships before..after only.
	write(t, dir, "a.go", "one\ntwo\nthree\n")
	second, err := svc.CommitPush(ctx, dir, "secret subject two")
	if err != nil {
		t.Fatal(err)
	}
	if got := shas(second.Shipped); len(got) != 1 || got[0] != second.Commit {
		t.Fatalf("second push shipped %v, want [%s]", got, second.Commit)
	}
	c := second.Shipped.Commits[0]
	if c.LinesAdded != 2 || c.LinesDeleted != 0 || c.FileChanges != 1 || len(c.Day) != len("2006-01-02") {
		t.Fatalf("commit counts: %+v", c)
	}
	if first.Shipped.EventID == second.Shipped.EventID {
		t.Fatal("event id reused across pushes")
	}

	// Nothing new: up to date, nothing shipped.
	upToDate, shipped, err := svc.Push(ctx, dir, "main")
	if err != nil || !upToDate || shipped != nil {
		t.Fatalf("up-to-date push: %v, %+v, %v", upToDate, shipped, err)
	}

	// A new branch ships only commits origin had on no other ref.
	if err := svc.CreateBranch(ctx, dir, "feature", "HEAD"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "b.go", "feature\n")
	feature, err := svc.CommitPush(ctx, dir, "secret subject three")
	if err != nil {
		t.Fatal(err)
	}
	if got := shas(feature.Shipped); len(got) != 1 || got[0] != feature.Commit {
		t.Fatalf("new branch shipped %v, want [%s]", got, feature.Commit)
	}

	// A new branch with no commits of its own ships nothing.
	if err := svc.CreateBranch(ctx, dir, "empty", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if upToDate, shipped, err := svc.Push(ctx, dir, "empty"); err != nil || upToDate || shipped != nil {
		t.Fatalf("empty branch: %v, %+v, %v", upToDate, shipped, err)
	}

	for _, r := range []git.CommitPushResult{first, second, feature} {
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), "secret subject") || !strings.Contains(string(b), `"shipped":{"event_id"`) {
			t.Fatalf("wire shape: %s", b)
		}
	}
}

func TestShippedExcludesNoiseAndCaps(t *testing.T) {
	svc, dir := repo(t)
	withRemote(t, dir)
	ctx := context.Background()
	if _, _, err := svc.Push(ctx, dir, "main"); err != nil {
		t.Fatal(err)
	}

	// Only excluded files: the commit is dropped, as the server needs file_changes > 0.
	write(t, dir, "package-lock.json", lines(50))
	commit(t, dir, "lockfile")
	lockOnly := head(t, dir)

	// One file over the per-file cap, plus vendored and generated noise.
	write(t, dir, "big.go", lines(2500))
	for _, p := range []string{"node_modules/x.js", "vendor/y.go", "app.min.js", "api.pb.go"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, dir, p, lines(10))
	}
	commit(t, dir, "big")
	big := head(t, dir)

	// Eleven capped files exceed the per-commit cap.
	for i := range 11 {
		write(t, dir, fmt.Sprintf("f%d.go", i), lines(2001))
	}
	commit(t, dir, "huge")
	huge := head(t, dir)

	upToDate, shipped, err := svc.Push(ctx, dir, "main")
	if err != nil || upToDate || shipped == nil {
		t.Fatalf("push: %v, %+v, %v", upToDate, shipped, err)
	}
	bySHA := map[string]git.ShippedCommit{}
	for _, c := range shipped.Commits {
		bySHA[c.SHA] = c
	}
	if _, ok := bySHA[lockOnly]; ok {
		t.Fatal("a lockfile-only commit was counted")
	}
	if c := bySHA[big]; c.LinesAdded != 2000 || c.FileChanges != 1 {
		t.Fatalf("per-file cap and exclusions: %+v", c)
	}
	if c := bySHA[huge]; c.LinesAdded != 20_000 || c.FileChanges != 11 {
		t.Fatalf("per-commit cap: %+v", c)
	}
	if len(shipped.Commits) != 2 {
		t.Fatalf("shipped %d commits, want 2", len(shipped.Commits))
	}
}
