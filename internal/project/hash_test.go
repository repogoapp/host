package project

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// project is a temp folder holding files, by path under it.
func project(t *testing.T, files map[string]string) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		write(t, dir, name, body)
	}
	return dir
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The hash a list carries is the one detect_icon answers, so a device holding
// it gets not_modified.
func TestIconHashIsDetectIconsContentHash(t *testing.T) {
	dir := project(t, map[string]string{"public/apple-touch-icon.png": "png"})
	s := service(dir, repo{})
	got, err := s.DetectIcon(context.Background(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if hash := s.IconHash(dir); hash == "" || hash != got.ContentHash {
		t.Fatalf("IconHash = %q, detect_icon content_hash = %q", hash, got.ContentHash)
	}
}

func TestIconHashIsEmptyWithoutAnIcon(t *testing.T) {
	dir := project(t, map[string]string{"README.md": "hi"})
	if hash := service(dir, repo{}).IconHash(dir); hash != "" {
		t.Fatalf("IconHash = %q, want none", hash)
	}
}

// An icon written in place, one added that outranks it, and one removed each
// move the hash, though the cache answers an unchanged project.
func TestIconHashFollowsTheIcon(t *testing.T) {
	dir := project(t, map[string]string{"favicon.ico": "ico"})
	s := service(dir, repo{})
	first := s.IconHash(dir)
	if first == "" || s.IconHash(dir) != first {
		t.Fatalf("IconHash = %q, then %q", first, s.IconHash(dir))
	}

	// Same folder listing, new bytes: only the file's own stat shows it.
	write(t, dir, "favicon.ico", "ico, redrawn")
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "favicon.ico"), later, later); err != nil {
		t.Fatal(err)
	}
	edited := s.IconHash(dir)
	if edited == first || edited == "" {
		t.Fatalf("an edited icon kept hash %q", edited)
	}

	write(t, dir, ".repogo/icon.png", "chosen")
	chosen := s.IconHash(dir)
	if chosen == edited || chosen == "" {
		t.Fatalf("a .repogo icon kept hash %q", chosen)
	}

	if err := os.RemoveAll(filepath.Join(dir, ".repogo")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "favicon.ico")); err != nil {
		t.Fatal(err)
	}
	if hash := s.IconHash(dir); hash != "" {
		t.Fatalf("no icon left, hash %q", hash)
	}
}
