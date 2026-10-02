package git

import (
	"os"
	"path/filepath"
	"strings"
)

// These read `.git` off the disk without running git: they sit on paths that
// run for every file event or every turn.

// Root is the nearest folder at or above dir holding `.git`, or "" outside git.
func Root(dir string) string {
	dir = filepath.Clean(dir)
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// Dir is where a checkout's metadata lives: a linked worktree's `.git` is a
// file holding `gitdir: <path>`.
func Dir(checkout string) string {
	dotGit := filepath.Join(checkout, ".git")
	info, err := os.Stat(dotGit)
	if err != nil || info.IsDir() {
		return dotGit
	}
	raw, err := os.ReadFile(dotGit)
	if err != nil {
		return dotGit
	}
	pointer, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	pointer = strings.TrimSpace(pointer)
	if !ok || pointer == "" {
		return dotGit
	}
	if !filepath.IsAbs(pointer) {
		pointer = filepath.Join(checkout, pointer)
	}
	return filepath.Clean(pointer)
}

// MainCheckout is the clone a linked worktree belongs to, from its gitdir
// `<main>/.git/worktrees/<name>`; any other checkout is its own.
func MainCheckout(checkout string) string {
	sep := string(filepath.Separator)
	main, _, found := strings.Cut(Dir(checkout), sep+filepath.Join(".git", "worktrees")+sep)
	if !found {
		return checkout
	}
	return main
}
