package files_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/repogo/host/internal/files"
)

func TestMutationLifecycleAndRootProtection(t *testing.T) {
	svc, root, outside := project(t)
	folder := filepath.Join(root, "new")
	if err := svc.Mkdir(folder); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "src", "main.go")
	target := filepath.Join(folder, "renamed.go")
	if err := svc.Rename(source, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("source remains: %v", err)
	}
	if err := svc.Rename(target, outside); err == nil {
		t.Fatal("rename escaped")
	}
	if err := svc.Delete(root); err == nil {
		t.Fatal("removed project root")
	}
	if err := svc.Rename(root, root+"-moved"); err == nil {
		t.Fatal("moved project root")
	}
	if err := svc.Delete(folder); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(folder); !os.IsNotExist(err) {
		t.Fatalf("folder remains: %v", err)
	}
	if err := svc.Delete(folder); !errors.Is(err, files.ErrNotFound) {
		t.Fatalf("missing delete: %v", err)
	}
}

func TestMutationSymlinkContainment(t *testing.T) {
	svc, root, outside := project(t)
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(link); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("symlink target removed: %v", err)
	}
	if err := os.Symlink(filepath.Dir(outside), link); err != nil {
		t.Fatal(err)
	}
	if err := svc.Mkdir(filepath.Join(link, "new")); !errors.Is(err, files.ErrOutsideRoots) {
		t.Fatalf("mkdir escaped: %v", err)
	}
	if err := svc.Delete(filepath.Join(link, filepath.Base(outside))); !errors.Is(err, files.ErrOutsideRoots) {
		t.Fatalf("delete escaped: %v", err)
	}
	if err := svc.Rename(filepath.Join(root, "src", "main.go"), filepath.Join(link, "new")); !errors.Is(err, files.ErrOutsideRoots) {
		t.Fatalf("rename escaped: %v", err)
	}
}

func TestRenameRefusesOverwriteAndNestedRootDeletion(t *testing.T) {
	svc, root, _ := project(t)
	source := filepath.Join(root, "src", "main.go")
	target := filepath.Join(root, "keep")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.Rename(source, target); !errors.Is(err, files.ErrInvalidOperation) {
		t.Fatalf("overwrite: %v", err)
	}
	nested := files.New(files.Config{Roots: files.StaticRoots{root, filepath.Join(root, "src")}})
	if err := nested.Delete(filepath.Join(root, "src")); !errors.Is(err, files.ErrInvalidOperation) {
		t.Fatalf("nested root: %v", err)
	}
}

func TestCreateRefusesExistingFile(t *testing.T) {
	svc, root, outside := project(t)
	fresh := filepath.Join(root, "src", "new.go")
	if _, err := svc.Create(fresh, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(fresh); err != nil || string(data) != "new" {
		t.Fatalf("created: %q, %v", data, err)
	}
	existing := filepath.Join(root, "src", "main.go")
	for _, path := range []string{fresh, existing, filepath.Join(root, "src")} {
		if _, err := svc.Create(path, []byte("clobber")); !errors.Is(err, files.ErrInvalidOperation) {
			t.Fatalf("create over %s: %v", path, err)
		}
	}
	if data, _ := os.ReadFile(existing); string(data) != "package main\n" {
		t.Fatalf("existing file changed: %q", data)
	}
	if _, err := svc.Create(filepath.Join(filepath.Dir(outside), "new"), nil); !errors.Is(err, files.ErrOutsideRoots) {
		t.Fatalf("create outside: %v", err)
	}
	// Only the file itself is left behind, no temp twin.
	entries, err := os.ReadDir(filepath.Join(root, "src"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("src holds %v, %v", entries, err)
	}
}

func TestSearchWireSemanticsAndBounds(t *testing.T) {
	svc, root, outside := project(t)
	if err := os.WriteFile(filepath.Join(root, "unicode.txt"), []byte("first\n😀 HELLO hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside.txt")); err != nil {
		t.Fatal(err)
	}
	result, err := svc.Search(root, "hello", "content", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 1 || result.Results[0].Path != "unicode.txt" {
		t.Fatalf("results: %+v", result)
	}
	match := result.Results[0].Matches[0]
	if match.Line != 1 || len(match.Ranges) != 2 || match.Ranges[0].Start != 3 || match.Ranges[0].End != 8 {
		t.Fatalf("UTF-16/line: %+v", match)
	}
	limited, err := svc.Search(root, "hello", "content", 1, false)
	if err != nil || !limited.Truncated || len(limited.Results[0].Matches[0].Ranges) != 1 {
		t.Fatalf("limit: %+v %v", limited, err)
	}
	sensitive, err := svc.Search(root, "HELLO", "content", 10, true)
	if err != nil || len(sensitive.Results[0].Matches[0].Ranges) != 1 {
		t.Fatalf("case-sensitive: %+v %v", sensitive, err)
	}
	names, err := svc.Search(root, "MAIN", "filename", 10, false)
	if err != nil || len(names.Results) != 1 || names.Results[0].Path != "src/main.go" {
		t.Fatalf("names: %+v %v", names, err)
	}
	escaped, err := svc.Search(root, "PRIVATE", "content", 10, false)
	if err != nil || len(escaped.Results) != 0 {
		t.Fatalf("symlink search: %+v %v", escaped, err)
	}
	if _, err := svc.Search(filepath.Dir(root), "a", "filename", 10, false); !errors.Is(err, files.ErrOutsideRoots) {
		t.Fatalf("search escaped: %v", err)
	}
	if _, err := svc.Search(root, "a", "regex", 10, false); !errors.Is(err, files.ErrInvalidOperation) {
		t.Fatalf("invalid mode: %v", err)
	}
}
