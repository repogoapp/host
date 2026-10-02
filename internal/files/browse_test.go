package files_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogo/host/internal/files"
)

// picker returns a service whose home holds a project folder, a file, and the
// folders the picker must never show, plus a folder outside home.
func picker(t *testing.T) (svc *files.Service, home, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home, outside = filepath.Join(base, "home"), filepath.Join(base, "outside")
	for _, dir := range []string{
		filepath.Join(home, "Desktop", "app"), filepath.Join(home, ".ssh"),
		filepath.Join(home, "Library", "Keychains"), outside,
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "notes.txt"), []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	return files.New(files.Config{Roots: files.StaticRoots{}, Home: home}), home, outside
}

var dirsOnly = files.BrowseOptions{DirsOnly: true}

func names(entries []files.Entry) []string {
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.Name
	}
	return out
}

func TestBrowseDirsOnlyStartsAtHomeWithFolderNamesOnly(t *testing.T) {
	svc, home, _ := picker(t)
	dir, entries, err := svc.Browse("", dirsOnly)
	if err != nil {
		t.Fatal(err)
	}
	if dir != home {
		t.Fatalf("listed %s, want home %s", dir, home)
	}
	// No notes.txt, no .ssh, no Library.
	if got := names(entries); len(got) != 1 || got[0] != "Desktop" {
		t.Fatalf("home lists %v, want [Desktop]", got)
	}
}

func TestBrowseDirsOnlyRefusesWhatThePickerMayNotShow(t *testing.T) {
	svc, home, outside := picker(t)
	link := filepath.Join(home, "Desktop", "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		outside, filepath.Join(home, ".ssh"), filepath.Join(home, "Library", "Keychains"),
		filepath.Join(home, "Desktop", "..", ".."), link, "Desktop",
	} {
		if _, _, err := svc.Browse(path, dirsOnly); !errors.Is(err, files.ErrNotBrowsable) {
			t.Errorf("Browse(%s) = %v, want ErrNotBrowsable", path, err)
		}
	}
}

func TestBrowseDirsOnlyWithoutAHomeServesOnlyRoots(t *testing.T) {
	svc, root, _ := project(t)
	if _, _, err := svc.Browse("", dirsOnly); !errors.Is(err, files.ErrNotBrowsable) {
		t.Fatalf("Folders with no home = %v, want ErrNotBrowsable", err)
	}
	if _, entries, err := svc.Browse(root, dirsOnly); err != nil || len(entries) != 1 || entries[0].Name != "src" {
		t.Fatalf("Browse(root) = %v, %v; want [src]", names(entries), err)
	}
}

func TestBrowseDirsOnlyInsideARootShowsHiddenFolders(t *testing.T) {
	_, home, _ := picker(t)
	app := filepath.Join(home, "Desktop", "app")
	if err := os.Mkdir(filepath.Join(app, ".github"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc := files.New(files.Config{Roots: files.StaticRoots{app}, Home: home})
	if _, entries, err := svc.Browse(app, dirsOnly); err != nil || len(entries) != 1 || entries[0].Name != ".github" {
		t.Fatalf("Browse(root) = %v, %v; want [.github]", names(entries), err)
	}
}

func TestPickableRefusesHomeItself(t *testing.T) {
	svc, home, _ := picker(t)
	if _, err := svc.Pickable(home); !errors.Is(err, files.ErrNotBrowsable) {
		t.Fatalf("Pickable(home) = %v, want ErrNotBrowsable", err)
	}
	app := filepath.Join(home, "Desktop", "app")
	if got, err := svc.Pickable(filepath.Join(home, "Desktop", ".", "app")); err != nil || got != app {
		t.Fatalf("Pickable(app) = %q, %v; want %q", got, err, app)
	}
	if _, err := svc.Pickable(filepath.Join(home, ".ssh")); !errors.Is(err, files.ErrNotBrowsable) {
		t.Fatalf("Pickable(.ssh) = %v, want ErrNotBrowsable", err)
	}
}

func TestMakeFolderStaysWhereThePickerCanSee(t *testing.T) {
	svc, home, outside := picker(t)
	desktop := filepath.Join(home, "Desktop")
	path, err := svc.MakeFolder(desktop, "new-app")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() || path != filepath.Join(desktop, "new-app") {
		t.Fatalf("MakeFolder made %q (%v)", path, err)
	}
	if _, err := svc.MakeFolder(desktop, "new-app"); !errors.Is(err, files.ErrInvalidOperation) {
		t.Errorf("a second MakeFolder = %v, want ErrInvalidOperation", err)
	}
	for _, name := range []string{"", ".hidden", "..", "a/b"} {
		if _, err := svc.MakeFolder(desktop, name); !errors.Is(err, files.ErrInvalidOperation) {
			t.Errorf("MakeFolder(%q) = %v, want ErrInvalidOperation", name, err)
		}
	}
	for _, parent := range []string{outside, filepath.Join(home, ".ssh")} {
		if _, err := svc.MakeFolder(parent, "x"); !errors.Is(err, files.ErrNotBrowsable) {
			t.Errorf("MakeFolder in %s = %v, want ErrNotBrowsable", parent, err)
		}
	}
}

// The env source picker: files beside folders past the roots, and with Hidden
// the dotfiles and dot folders the folder picker leaves out, still under home.
func TestBrowseListsFilesAndHiddenUnderHome(t *testing.T) {
	svc, home, outside := picker(t)
	if err := os.WriteFile(filepath.Join(home, "Desktop", "app", ".env"), []byte("A=1"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, entries, err := svc.Browse("", files.BrowseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(entries), ","); got != "Desktop,notes.txt" {
		t.Fatalf("home lists %s, want Desktop,notes.txt", got)
	}
	if entries[1].Size != 2 || entries[1].Dir {
		t.Fatalf("notes.txt = %+v", entries[1])
	}
	if _, entries, err := svc.Browse(filepath.Join(home, "Desktop", "app"), files.BrowseOptions{}); err != nil || len(entries) != 0 {
		t.Fatalf("without Hidden, app lists %v, %v; want nothing", names(entries), err)
	}

	withHidden := files.BrowseOptions{Hidden: true}
	if _, entries, err := svc.Browse("", withHidden); err != nil || strings.Join(names(entries), ",") != ".ssh,Desktop,Library,notes.txt" {
		t.Fatalf("home with Hidden lists %v, %v", names(entries), err)
	}
	if _, entries, err := svc.Browse(filepath.Join(home, "Desktop", "app"), withHidden); err != nil || len(entries) != 1 || entries[0].Name != ".env" {
		t.Fatalf("app with Hidden lists %v, %v; want [.env]", names(entries), err)
	}
	if _, _, err := svc.Browse(filepath.Join(home, ".ssh"), withHidden); err != nil {
		t.Fatalf("Hidden opens .ssh: %v", err)
	}
	link := filepath.Join(home, "Desktop", "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, link, filepath.Join(home, "notes.txt")} {
		if _, _, err := svc.Browse(path, withHidden); err == nil {
			t.Errorf("Browse(%s) with Hidden = nil, want refused", path)
		}
	}
}
