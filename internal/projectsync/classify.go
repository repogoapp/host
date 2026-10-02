package projectsync

import (
	"context"
	"os"
	"path/filepath"

	"github.com/repogo/host/internal/files"
	gitcore "github.com/repogo/host/internal/git"
	"github.com/repogo/host/internal/github"
	"github.com/repogo/host/internal/store"
)

// kindOf classifies from git's own invariant (a clone's `.git` is a directory,
// a linked worktree's is a file) plus the one directory this host creates
// worktrees in, so nothing needs recording.
func kindOf(path, checkouts string) store.ProjectKind {
	info, err := os.Lstat(filepath.Join(path, ".git"))
	switch {
	case err != nil:
		return store.ProjectFolder
	case info.IsDir():
		return store.ProjectClone
	case checkouts != "" && files.Within(checkouts, path):
		return store.ProjectManaged
	default:
		return store.ProjectWorktree
	}
}

// repoOf is the repository a checkout belongs to, as the origin (or folder)
// spells it. The path layout first because it is free; origin costs a subprocess.
func repoOf(ctx context.Context, path, checkouts string) (owner, name string) {
	if owner, name, ok := github.CheckoutRepo(checkouts, path); ok {
		return owner, name
	}
	return gitcore.OwnerRepo(ctx, path)
}
