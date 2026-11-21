//go:build !(windows && cgo)

package desktop

import (
	"cmp"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Installed are the apps Launchpad shows: every app in the Applications
// folders, by name. Icons come separately, as the phone scrolls to them.
func Installed() []App {
	home, _ := os.UserHomeDir()
	dirs := []string{
		"/Applications", "/Applications/Utilities", "/System/Applications",
		"/System/Applications/Utilities", filepath.Join(home, "Applications"),
	}
	seen := map[string]bool{}
	var out []App
	for _, dir := range dirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".app") {
				continue
			}
			a, ok := bundleAt(filepath.Join(dir, e.Name()))
			if !ok || seen[a.Bundle] {
				continue
			}
			seen[a.Bundle] = true
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b App) int {
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out
}
