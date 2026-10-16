package projectsync

import (
	"context"
	"os"
	"path/filepath"

	gitcore "github.com/repogoapp/host/internal/git"
	"github.com/repogoapp/host/internal/store"
)

// kindOf is a clone when `.git` exists, a directory or a linked checkout's
// file, so nothing needs recording.
func kindOf(path string) store.ProjectKind {
	if _, err := os.Lstat(filepath.Join(path, ".git")); err != nil {
		return store.ProjectFolder
	}
	return store.ProjectClone
}

// repoOf is the repository a checkout belongs to, as its origin spells it.
func repoOf(ctx context.Context, path string) (owner, name string) {
	return gitcore.OwnerRepo(ctx, path)
}
