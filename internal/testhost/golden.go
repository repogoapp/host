package testhost

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Golden checks the generated file at path against want, or rewrites it when
// update is set; hint says how to regenerate it.
func Golden(t testing.TB, path string, want []byte, update bool, hint string) {
	t.Helper()
	if update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	have, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v — %s", err, hint)
	}
	if string(have) != string(want) {
		t.Errorf("%s is stale — %s\n%s", path, hint, missingLines(string(have), string(want)))
	}
}

// GoldenDir checks that dir holds exactly want's files, name to contents, or
// rewrites it when update is set.
func GoldenDir(t testing.TB, dir string, want map[string][]byte, update bool, hint string) {
	t.Helper()
	if update {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		for name, b := range want {
			Golden(t, filepath.Join(dir, name), b, true, hint)
		}
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("%v — %s", err, hint)
	}
	for _, e := range entries {
		if _, ok := want[e.Name()]; !ok {
			t.Errorf("%s/%s is not generated — %s", dir, e.Name(), hint)
		}
	}
	for name, b := range want {
		if have, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(have) != string(b) {
			t.Errorf("%s/%s is stale — %s", dir, name, hint)
		}
	}
}

// SkipWithoutClient skips a test of the iOS client's source when swiftDir is
// absent: the client sits beside the host in the RepoGo repo, not in a host-only clone.
func SkipWithoutClient(t testing.TB, swiftDir string) {
	t.Helper()
	if _, err := os.Stat(swiftDir); errors.Is(err, fs.ErrNotExist) {
		t.Skip("no iOS client at " + swiftDir)
	}
}

// SwiftSources calls fn with every .swift file under dirs.
func SwiftSources(t testing.TB, dirs []string, fn func(src []byte)) {
	t.Helper()
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() && (d.Name() == ".build" || d.Name() == "build" || d.Name() == "Tests") {
				return filepath.SkipDir
			}
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".swift") {
				return err
			}
			src, err := os.ReadFile(path)
			if err == nil {
				fn(src)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// missingLines is want's lines that have lacks, as a short diff.
func missingLines(have, want string) string {
	seen := map[string]bool{}
	for _, l := range strings.Split(have, "\n") {
		seen[l] = true
	}
	var missing []string
	for _, l := range strings.Split(want, "\n") {
		if !seen[l] {
			missing = append(missing, "+ "+l)
		}
	}
	sort.Strings(missing)
	return strings.Join(missing, "\n")
}
