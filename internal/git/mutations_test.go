package git_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/git"
)

func TestMutationsPreserveDirtyWorkAndValidateBranches(t *testing.T) {
	svc, dir := repo(t)
	ctx := context.Background()
	if err := svc.CreateBranch(ctx, dir, "feature", "HEAD"); err != nil {
		t.Fatal(err)
	}
	if err := svc.SwitchBranch(ctx, dir, "main"); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "README.md", "unsaved\n")
	for _, call := range []func() error{
		func() error { return svc.SwitchBranch(ctx, dir, "feature") },
		func() error { return svc.CreateBranch(ctx, dir, "dirty", "HEAD") },
		func() error { return svc.Pull(ctx, dir) },
		func() error { return svc.CreateBranch(ctx, dir, "--orphan", "HEAD") },
		func() error { return svc.SwitchBranch(ctx, dir, "@{-1}") },
		func() error { return svc.CreateBranch(ctx, dir, "safe", "--help") },
	} {
		if err := call(); !errors.Is(err, git.ErrInvalidMutation) {
			t.Fatalf("expected invalid operation, got %v", err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil || string(data) != "unsaved\n" {
		t.Fatalf("dirty work lost: %q, %v", data, err)
	}
}

func TestCommitPushPullAndResetWithTemporaryRemote(t *testing.T) {
	svc, dir := repo(t)
	ctx := context.Background()
	remote := t.TempDir()
	if _, err := git.Run(ctx, remote, "init", "--bare", "--initial-branch=main"); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, dir, "remote", "add", "origin", remote); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "README.md", "second\n")
	result, err := svc.CommitPush(ctx, dir, "second")
	if err != nil || !result.Pushed || result.Branch != "main" || result.Commit == "" {
		t.Fatalf("push: %+v, %v", result, err)
	}
	if err := svc.Pull(ctx, dir); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "README.md", "discard this\n")
	if err := svc.ResetHard(ctx, dir, "main"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "README.md"))
	if string(data) != "second\n" {
		t.Fatalf("reset: %q", data)
	}
	if _, err := git.Run(ctx, dir, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Fatal(err)
	}
	write(t, dir, "README.md", "third\n")
	result, err = svc.CommitPush(ctx, dir, "third")
	if err == nil || !strings.Contains(err.Error(), "saved locally") || result.Commit == "" || result.Pushed {
		t.Fatalf("push failure: %+v, %v", result, err)
	}
}

func TestMutationCannotReachParentRepositoryOutsideRoots(t *testing.T) {
	_, dir := repo(t)
	child := filepath.Join(dir, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	svc := git.New(files.New(files.Config{Roots: files.StaticRoots{child}}), slog.New(slog.DiscardHandler))
	if err := svc.CreateBranch(context.Background(), child, "escape", "HEAD"); !errors.Is(err, files.ErrOutsideRoots) {
		t.Fatalf("parent escape: %v", err)
	}
}
