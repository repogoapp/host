package files_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/repogo/host/internal/files"
)

// deleting returns a service whose one root is home/RepoGo/app, with a Trash
// in home when trash is set.
func deleting(t *testing.T, trash bool) (svc *files.Service, home, app string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home = filepath.Join(base, "home")
	projects := filepath.Join(home, "RepoGo")
	app = filepath.Join(projects, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "main.go"), []byte("package main"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := files.Config{Roots: files.StaticRoots{app, projects, home}, Home: home, Projects: projects}
	if trash {
		cfg.Trash = filepath.Join(home, ".Trash")
	}
	return files.New(cfg), home, app
}

func TestDeleteProjectMovesItToTheTrash(t *testing.T) {
	svc, home, app := deleting(t, true)
	if err := svc.DeleteProject(app); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(app); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the project is still there: %v", err)
	}
	trashed, err := filepath.Glob(filepath.Join(home, ".Trash", "app *", "main.go"))
	if err != nil || len(trashed) != 1 {
		t.Fatalf("Trash holds %v (%v), want the project's main.go once", trashed, err)
	}

	// A second project of the same name lands beside the first.
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteProject(app); err != nil {
		t.Fatal(err)
	}
	if both, _ := filepath.Glob(filepath.Join(home, ".Trash", "app *")); len(both) != 2 {
		t.Fatalf("Trash holds %v, want two", both)
	}
}

func TestDeleteProjectWithNoTrashDeletesIt(t *testing.T) {
	svc, _, app := deleting(t, false)
	if err := svc.DeleteProject(app); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(app); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the project is still there: %v", err)
	}
}

// Home and the projects folder hold every project, and a folder inside a
// project is not one.
func TestDeleteProjectRefusesWhatIsNotOneProject(t *testing.T) {
	svc, home, app := deleting(t, true)
	inside := filepath.Join(app, "src")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{home, filepath.Join(home, "RepoGo"), inside, "RepoGo/app"} {
		if err := svc.DeleteProject(path); err == nil {
			t.Errorf("DeleteProject(%s) deleted it", path)
		}
	}
	if _, err := os.Stat(filepath.Join(app, "main.go")); err != nil {
		t.Fatalf("a refused delete touched the project: %v", err)
	}
}

func TestDeleteMovesAFileOrFolderToTheTrash(t *testing.T) {
	svc, home, app := deleting(t, true)
	if err := svc.Delete(filepath.Join(app, "main.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(app, "main.go")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the file is still there: %v", err)
	}
	if trashed, _ := filepath.Glob(filepath.Join(home, ".Trash", "main.go *")); len(trashed) != 1 {
		t.Fatalf("Trash holds %v, want main.go once", trashed)
	}

	folder := filepath.Join(app, "src")
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(folder, "lib.go"), []byte("package src"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(folder); err != nil {
		t.Fatal(err)
	}
	if trashed, _ := filepath.Glob(filepath.Join(home, ".Trash", "src *", "lib.go")); len(trashed) != 1 {
		t.Fatalf("Trash holds %v, want the folder's lib.go once", trashed)
	}
}

// A symlink goes to the Trash as a link; what it points at stays put.
func TestDeleteTrashesASymlinkNotItsTarget(t *testing.T) {
	svc, home, app := deleting(t, true)
	target := filepath.Join(home, "notes.txt")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(app, "link")); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(filepath.Join(app, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("the link's target went too: %v", err)
	}
	trashed, _ := filepath.Glob(filepath.Join(home, ".Trash", "link *"))
	if len(trashed) != 1 {
		t.Fatalf("Trash holds %v, want the link once", trashed)
	}
	if info, err := os.Lstat(trashed[0]); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("trashed link: %v, %v", info, err)
	}
}
