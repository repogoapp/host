package files

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestPinnedParentSurvivesPathReplacement(t *testing.T) {
	base, outside := t.TempDir(), t.TempDir()
	parent := filepath.Join(base, "parent")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{filepath.Join(parent, "file"): "inside", filepath.Join(outside, "file"): "outside"} {
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := New(Config{Roots: StaticRoots{base}})
	p, err := s.filePath(filepath.Join(parent, "file"), Scope{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.root.Close()
	if err := os.Rename(parent, parent+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	f, err := p.open()
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := io.ReadAll(f)
	if err != nil || string(got) != "inside" {
		t.Fatalf("read %q, %v", got, err)
	}
	if err := p.root.WriteFile(p.name, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(outside, "file"))
	if err != nil || string(got) != "outside" {
		t.Fatalf("outside file %q, %v", got, err)
	}
}

func TestPinnedReadRefusesReplacedLeaf(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "file.md")
	if err := os.WriteFile(path, []byte("inside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "private.json"), []byte("private"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Roots: StaticRoots{base}})
	p, err := s.filePath(path, Scope{}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer p.root.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("private.json", path); err != nil {
		t.Fatal(err)
	}
	if f, err := p.open(); err == nil {
		f.Close()
		t.Fatal("followed replacement symlink")
	}
}
