package projectwatch

import (
	"path/filepath"
	"slices"
	"strings"

	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/git"
)

// skipDirs churn on every build and hold nothing a diff shows. Other ignored
// files cost one git run, and the payload dedupe keeps that run off the wire.
var skipDirs = map[string]bool{
	"node_modules": true, "dist": true, ".next": true, ".cache": true, ".turbo": true,
	"coverage": true, ".swc": true, ".expo": true, "DerivedData": true,
}

// watchTree calls kick per change a diff or an editor would show: the file
// relative to the folder ("" for git's own state) and whether it was created or
// renamed. False where there is no tree watcher, and the caller polls instead.
func watchTree(path string, kick func(file string, created bool)) (stop func(), ok bool) {
	root, gitdir := resolved(path), resolved(git.Dir(path))
	paths := []string{root}
	if !files.Within(root, gitdir) {
		paths = append(paths, gitdir)
	}
	return watchEvents(paths, func(changed string, created bool) {
		if relevant(root, gitdir, changed) {
			kick(worktreeFile(root, gitdir, changed), created)
		}
	})
}

// worktreeFile is changed relative to root, or "" when it is git's own state
// or the checkout folder itself.
func worktreeFile(root, gitdir, changed string) string {
	if files.Within(gitdir, changed) {
		return ""
	}
	if file := rel(root, changed); file != "." {
		return file
	}
	return ""
}

// relevant keeps gitFiles and any worktree file outside skipDirs. Objects and
// logs move with those and add nothing.
func relevant(root, gitdir, path string) bool {
	if files.Within(gitdir, path) {
		first, _, _ := strings.Cut(rel(gitdir, path), "/")
		return slices.Contains(gitFiles, first)
	}
	if !files.Within(root, path) {
		return false
	}
	for _, part := range strings.Split(rel(root, path), "/") {
		if skipDirs[part] {
			return false
		}
	}
	return filepath.Base(path) != ".DS_Store"
}

// rel is path below dir, slash-separated; files.Within has already proved it is.
func rel(dir, path string) string {
	r, _ := filepath.Rel(dir, path)
	return filepath.ToSlash(r)
}

// resolved matches the real paths FSEvents reports, e.g. /private/var for /var.
func resolved(path string) string {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return path
}
