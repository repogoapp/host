package files_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/files"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func markdown(root string) files.Scope {
	return files.Scope{Within: root, Extensions: []string{"md"}}
}

func TestListFilesFindsEveryMarkdownFileAndSkipsDependencies(t *testing.T) {
	svc, root, _ := project(t)
	write(t, filepath.Join(root, "README.md"), "# hi")
	write(t, filepath.Join(root, "docs", "Plan.MD"), "plan")
	write(t, filepath.Join(root, "node_modules", "pkg", "README.md"), "dependency")
	write(t, filepath.Join(root, "notes.txt"), "not markdown")
	if err := os.Symlink(filepath.Join(root, "README.md"), filepath.Join(root, "link.md")); err != nil {
		t.Fatal(err)
	}

	entries, truncated, err := svc.ListFiles(root, []string{"md"}, root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	if got := strings.Join(names, ","); got != "docs/Plan.MD,README.md" || truncated {
		t.Fatalf("names = %s truncated = %v, want docs/Plan.MD,README.md and not truncated", got, truncated)
	}
	if entries[1].Path != filepath.Join(root, "README.md") {
		t.Fatalf("path = %s, want the absolute path", entries[1].Path)
	}
}

func TestListFilesNeedsExtensionsAndARoot(t *testing.T) {
	svc, root, secret := project(t)
	if _, _, err := svc.ListFiles(root, nil, ""); !errors.Is(err, files.ErrInvalidOperation) {
		t.Fatalf("no extensions: err = %v", err)
	}
	if _, _, err := svc.ListFiles(filepath.Dir(secret), []string{"md"}, ""); !errors.Is(err, files.ErrOutsideRoots) {
		t.Fatalf("outside every root: err = %v", err)
	}
}

func TestReplaceChangesTheOnePlaceTheTextAppears(t *testing.T) {
	svc, root, _ := project(t)
	path := filepath.Join(root, "TODO.md")
	write(t, path, "- [ ] one\n- [ ] two\n")

	if _, err := svc.Replace(path, "- [ ] two", "- [x] two", files.WriteOptions{Scope: markdown(root)}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "- [ ] one\n- [x] two\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestReplaceRefusesMissingRepeatedAndEmptyText(t *testing.T) {
	svc, root, _ := project(t)
	path := filepath.Join(root, "TODO.md")
	write(t, path, "same\nsame\n")
	for _, old := range []string{"absent", "same", ""} {
		if _, err := svc.Replace(path, old, "x", files.WriteOptions{}); !errors.Is(err, files.ErrInvalidOperation) {
			t.Errorf("old %q: err = %v, want ErrInvalidOperation", old, err)
		}
	}
	if got, _ := os.ReadFile(path); string(got) != "same\nsame\n" {
		t.Fatalf("a refused replace changed the file: %q", got)
	}
}

func TestReplaceKeepsWhitespaceAndMayDelete(t *testing.T) {
	svc, root, _ := project(t)
	path := filepath.Join(root, "notes.md")
	write(t, path, "keep\n\n  drop me  \nkeep\n")
	if _, err := svc.Replace(path, "\n  drop me  \n", "\n", files.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(path); string(got) != "keep\n\nkeep\n" {
		t.Fatalf("content = %q", got)
	}
}

func TestAChangedFileIsRefusedNotOverwritten(t *testing.T) {
	svc, root, _ := project(t)
	path := filepath.Join(root, "plan.md")
	write(t, path, "first")
	read, err := svc.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	stale := read.ModTime - 1000

	if _, err := svc.WriteWith(path, []byte("mine"), files.WriteOptions{ModTime: stale}); !errors.Is(err, files.ErrChanged) {
		t.Fatalf("write: err = %v, want ErrChanged", err)
	}
	if _, err := svc.Replace(path, "first", "mine", files.WriteOptions{ModTime: stale}); !errors.Is(err, files.ErrChanged) {
		t.Fatalf("replace: err = %v, want ErrChanged", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "first" {
		t.Fatalf("content = %q, want it untouched", got)
	}
	if _, err := svc.WriteWith(path, []byte("mine"), files.WriteOptions{ModTime: read.ModTime}); err != nil {
		t.Fatalf("write with the mod_time read: %v", err)
	}
}

func TestScopeHoldsToOneFolderAndOneKind(t *testing.T) {
	svc, root, _ := project(t)
	other := filepath.Join(filepath.Dir(root), "other")
	write(t, filepath.Join(other, "secret.md"), "other project")
	svc = files.New(files.Config{Roots: files.StaticRoots{root, other}})
	write(t, filepath.Join(root, "config.json"), "{}")
	if err := os.Symlink(filepath.Join(root, "config.json"), filepath.Join(root, "notes.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "secret.md"), filepath.Join(root, "away.md")); err != nil {
		t.Fatal(err)
	}
	scope := markdown(root)

	for _, path := range []string{
		filepath.Join(root, "config.json"),    // not markdown
		filepath.Join(root, "notes.md"),       // markdown name, json file
		filepath.Join(root, "away.md"),        // a link into another project
		filepath.Join(other, "secret.md"),     // another project
		filepath.Join(root, "src", "main.go"), // not markdown
	} {
		if _, err := svc.ReadIn(path, scope); !errors.Is(err, files.ErrOutsideScope) {
			t.Errorf("read %s: err = %v, want ErrOutsideScope", path, err)
		}
		if _, err := svc.WriteWith(path, []byte("x"), files.WriteOptions{Scope: scope}); !errors.Is(err, files.ErrOutsideScope) {
			t.Errorf("write %s: err = %v, want ErrOutsideScope", path, err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(root, "config.json")); string(got) != "{}" {
		t.Fatalf("config.json = %q, want it untouched", got)
	}
	if _, err := svc.WriteWith(filepath.Join(root, "new.md"), []byte("# new"), files.WriteOptions{Scope: scope}); err != nil {
		t.Fatalf("a new markdown file in the folder: %v", err)
	}
}

func TestOnlyAScopedWriteIsHeldToTheReadLimit(t *testing.T) {
	svc, root, _ := project(t)
	big := make([]byte, files.MaxFileBytes+1)
	if _, err := svc.WriteWith(filepath.Join(root, "big.md"), big, files.WriteOptions{Scope: markdown(root)}); !errors.Is(err, files.ErrTooLarge) {
		t.Fatalf("scoped: err = %v, want ErrTooLarge", err)
	}
	if _, err := svc.Write(filepath.Join(root, "big.bin"), big); err != nil {
		t.Fatalf("unscoped: %v", err)
	}
}

func TestListFilesStaysWithinItsProject(t *testing.T) {
	svc, root, _ := project(t)
	other := filepath.Join(filepath.Dir(root), "other")
	write(t, filepath.Join(other, "secret.md"), "other project")
	svc = files.New(files.Config{Roots: files.StaticRoots{root, other}})
	if err := os.Symlink(other, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ListFiles(filepath.Join(root, "linked"), []string{"md"}, root); !errors.Is(err, files.ErrOutsideScope) {
		t.Fatalf("err = %v, want ErrOutsideScope", err)
	}
}

func TestReplaceCountsOverlappingText(t *testing.T) {
	svc, root, _ := project(t)
	path := filepath.Join(root, "notes.md")
	write(t, path, "aaa")
	if _, err := svc.Replace(path, "aa", "b", files.WriteOptions{}); !errors.Is(err, files.ErrInvalidOperation) {
		t.Fatalf("err = %v, want the overlapping text refused", err)
	}
}

// An open tab re-reads on every change push; an unchanged file must cost a
// stat, not its bytes, and must still answer to scope.
func TestReadChangedSkipsTheContentOfAnUnchangedFile(t *testing.T) {
	svc, root, _ := project(t)
	path := filepath.Join(root, "notes.md")
	write(t, path, "first")
	first, changed, err := svc.ReadChanged(path, files.Scope{}, 0)
	if err != nil || !changed || string(first.Content) != "first" {
		t.Fatalf("a read without mod_time = %q, changed %v, %v; want the content", first.Content, changed, err)
	}

	same, changed, err := svc.ReadChanged(path, files.Scope{}, first.ModTime)
	if err != nil || changed || same.Content != nil || same.ModTime != first.ModTime || same.Size != first.Size {
		t.Fatalf("an unchanged read = %+v, changed %v, %v; want no content and the same stat", same, changed, err)
	}

	write(t, path, "second")
	later := time.UnixMilli(first.ModTime).Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	next, changed, err := svc.ReadChanged(path, files.Scope{}, first.ModTime)
	if err != nil || !changed || string(next.Content) != "second" {
		t.Fatalf("a changed read = %q, changed %v, %v; want the new content", next.Content, changed, err)
	}

	scope := files.Scope{Within: root, Extensions: []string{"txt"}}
	if _, _, err := svc.ReadChanged(path, scope, next.ModTime); !errors.Is(err, files.ErrOutsideScope) {
		t.Errorf("an unchanged read outside scope: err = %v, want ErrOutsideScope", err)
	}
}
