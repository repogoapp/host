package session

import (
	"os"
	"path/filepath"
	"strings"
)

// excludedRoots are the OS temp roots, where tests and probes leave
// transcripts, raw and resolved: a transcript records the path the CLI saw,
// and on a Mac /tmp and /var are symlinks into /private.
var excludedRoots = tempRoots()

func tempRoots() []string {
	seen := map[string]bool{}
	var out []string
	add := func(dir string) {
		dir = filepath.Clean(dir)
		if dir == "" || dir == "/" || dir == "." || seen[dir] {
			return
		}
		seen[dir] = true
		out = append(out, dir)
	}
	for _, dir := range []string{os.TempDir(), "/tmp", "/private/tmp", "/var/folders", "/private/var/folders"} {
		add(dir)
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			add(resolved)
		}
	}
	return out
}

// isExcluded reports whether a cwd lives under one of the excluded roots. A
// prefix match on the roots themselves, not a name pattern: the next tool to
// call os.MkdirTemp would slip past a pattern.
func isExcluded(cwd string) bool {
	if cwd == "" {
		return false
	}
	cwd = filepath.Clean(cwd)
	for _, root := range excludedRoots {
		if cwd == root || strings.HasPrefix(cwd, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
