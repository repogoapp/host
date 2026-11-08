package files_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/repogoapp/host/internal/files"
)

// A file under home outside every project reads, as an image an agent made in
// a scratch folder does; hidden folders, folders, writes and listing do not.
func TestReadsOneFileUnderHomeOutsideProjects(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "project")
	write := func(rel, body string) string {
		path := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	write("project/main.go", "package main\n")
	sheet := write("plan22/sheet/page-1.jpg", "jpg")
	key := write(".ssh/id_ed25519", "PRIVATE KEY")
	hiddenFile := write("plan22/.env", "TOKEN=1")
	link := filepath.Join(home, "plan22", "key")
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}

	svc := files.New(files.Config{Roots: files.StaticRoots{root}, Home: home})

	file, err := svc.Read(sheet)
	if err != nil || string(file.Content) != "jpg" {
		t.Fatalf("read under home: %v %q", err, file.Content)
	}
	if file, err := svc.Read("~/plan22/sheet/page-1.jpg"); err != nil || string(file.Content) != "jpg" {
		t.Fatalf("read ~/: %v", err)
	}
	for name, path := range map[string]string{
		"hidden folder": key, "hidden file": hiddenFile, "link into .ssh": link,
		"a folder": filepath.Dir(sheet), "home itself": home, "outside home": "/etc/hosts",
	} {
		if _, err := svc.Read(path); !errors.Is(err, files.ErrOutsideRoots) {
			t.Errorf("%s: %v, want ErrOutsideRoots", name, err)
		}
	}
	if _, err := svc.List(filepath.Dir(sheet)); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("list under home: %v, want ErrOutsideRoots", err)
	}
	if _, err := svc.Write(sheet, []byte("x")); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("write under home: %v, want ErrOutsideRoots", err)
	}
}
