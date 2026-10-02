package projectwatch

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// maxFiles bounds one fs.change; past it the push says Truncated and the
// device re-reads everything it has open. A checkout or an install moves
// thousands of files and no one needs their names.
const maxFiles = 200

// maxCollected bounds one settle before git is asked which files it ignores,
// so a first burst of build output can be dropped before maxFiles applies.
const maxCollected = 2000

// changedFiles collects the worktree files FSEvents reports between two
// settles, and whether any report said the file was created or renamed.
// FSEvents calls back on its own queue, hence the lock.
type changedFiles struct {
	mu        sync.Mutex
	files     map[string]bool
	truncated bool
	// ignoredDirs are folders git ignores, each with a trailing slash. Files
	// under them are dropped as they are reported, so they never fill a settle.
	ignoredDirs map[string]bool
}

func (c *changedFiles) add(file string, created bool) {
	if file == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// New rules may ignore different folders; the next settle asks git again.
	if filepath.Base(file) == ".gitignore" {
		c.ignoredDirs = nil
	}
	if c.underIgnoredDir(file) {
		return
	}
	if c.files == nil {
		c.files = map[string]bool{}
	}
	if seen, ok := c.files[file]; ok {
		c.files[file] = seen || created
		return
	}
	if len(c.files) >= maxCollected {
		c.truncated = true
		return
	}
	c.files[file] = created
}

// ignoreDirs remembers folders git reported ignored.
func (c *changedFiles) ignoreDirs(dirs []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ignoredDirs == nil {
		c.ignoredDirs = map[string]bool{}
	}
	for _, dir := range dirs {
		c.ignoredDirs[dir] = true
	}
}

// underIgnoredDir is called with c.mu held.
func (c *changedFiles) underIgnoredDir(file string) bool {
	for _, dir := range parentDirs(file) {
		if c.ignoredDirs[dir] {
			return true
		}
	}
	return false
}

// parentDirs is every folder above file, outermost first, each with the
// trailing slash git uses to name a folder: "a/b/c.go" is "a/", "a/b/".
func parentDirs(file string) []string {
	var dirs []string
	for i, r := range file {
		if r == '/' {
			dirs = append(dirs, file[:i+1])
		}
	}
	return dirs
}

// take returns what was collected and starts over.
func (c *changedFiles) take() (files map[string]bool, truncated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	files, truncated = c.files, c.truncated
	c.files, c.truncated = nil, false
	return files, truncated
}

// classify names each change from the disk now: FSEvents' flags coalesce, so a
// save by rename reads as created and removed at once. Gone is a delete, made
// or renamed into place a create, the rest updates; sorted by file.
func classify(root string, files map[string]bool) []FileChange {
	changes := make([]FileChange, 0, len(files))
	for file, created := range files {
		kind := KindUpdate
		if _, err := os.Lstat(filepath.Join(root, file)); errors.Is(err, fs.ErrNotExist) {
			kind = KindDelete
		} else if created {
			kind = KindCreate
		}
		changes = append(changes, FileChange{File: file, Kind: kind})
	}
	slices.SortFunc(changes, func(a, b FileChange) int { return cmp.Compare(a.File, b.File) })
	return changes
}

// dropIgnored removes files inside folders git ignores, and remembers those
// folders so later reports under them stop at add. An ignored file outside
// them, such as .env, still syncs. On a git failure every file is kept.
func (m *Manager) dropIgnored(ctx context.Context, path string, moved *changedFiles, files map[string]bool) map[string]bool {
	var asked []string
	seen := map[string]bool{}
	for file := range files {
		for _, dir := range parentDirs(file) {
			if !seen[dir] {
				seen[dir] = true
				asked = append(asked, dir)
			}
		}
	}
	if len(asked) == 0 {
		return files
	}
	ignored, err := m.git.Ignored(ctx, path, asked)
	if err != nil {
		m.log.Debug("watch: check-ignore failed", "path", path, "err", err)
		return files
	}
	moved.ignoreDirs(slices.Collect(maps.Keys(ignored)))

	kept := make(map[string]bool, len(files))
	for file, created := range files {
		if !slices.ContainsFunc(parentDirs(file), func(dir string) bool { return ignored[dir] }) {
			kept[file] = created
		}
	}
	return kept
}

// capped keeps the first maxFiles changes, and says whether any were cut.
func capped(changes []FileChange, truncated bool) ([]FileChange, bool) {
	if len(changes) > maxFiles {
		return changes[:maxFiles], true
	}
	return changes, truncated
}

// sendChanges tells the room which files moved. Nothing is sent for a settle
// that moved only git's own state.
func (m *Manager) sendChanges(path string, changes []FileChange, truncated bool) {
	if len(changes) == 0 && !truncated {
		return
	}
	payload, err := json.Marshal(Change{Path: path, Changes: changes, Truncated: truncated})
	if err != nil {
		return
	}
	for _, id := range m.subscribers(path) {
		if err := m.pub.Send(id, Change{}.Method(), payload); err != nil {
			m.log.Debug("watch: fs.change push failed", "device", id, "err", err)
		}
	}
}
