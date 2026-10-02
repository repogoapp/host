package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/repogo/host/internal/files"
)

// repo is a Repos answering one owner/name for every path, or err.
type repo struct {
	owner, name string
	err         error
}

func (r repo) Repo(string) (string, string, error) { return r.owner, r.name, r.err }

func service(dir string, r repo) *Service {
	return New(files.New(files.Config{Roots: files.StaticRoots{dir}}), r)
}

func TestDetectIconPrefersAppleTouchAndHonoursIfNoneMatch(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "public"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"favicon.ico": "ico", "public/apple-touch-icon.png": "png"} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := service(dir, repo{owner: "octocat", name: "hello"})

	got, err := s.DetectIcon(context.Background(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "public/apple-touch-icon.png" || got.ContentType != "image/png" || string(got.Bytes) != "png" {
		t.Fatalf("icon = %+v, want the apple touch icon with its bytes", got)
	}
	if got.Remote != "octocat/hello" || got.ContentHash == "" {
		t.Errorf("remote/hash = %q/%q", got.Remote, got.ContentHash)
	}

	again, err := s.DetectIcon(context.Background(), dir, got.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	if !again.NotModified || again.Bytes != nil {
		t.Errorf("a matching hash returned %+v, want not_modified and no bytes", again)
	}
}

func TestDetectIconWithoutOneStillReportsTheRemote(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := service(dir, repo{owner: "octocat", name: "hello"}).DetectIcon(context.Background(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "" || got.Remote != "octocat/hello" {
		t.Errorf("result = %+v, want no icon and the remote", got)
	}
}

func TestDetectIconPrefersALargeRepogoIconOverEveryOther(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{".repogo", "public"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Past the 1 MB cap guessed icons get, as a 1024px PNG often is.
	large := make([]byte, iconMaxBytes+1)
	for name, body := range map[string][]byte{
		".repogo/icon.png":            large,
		"public/apple-touch-icon.png": []byte("png"),
		"favicon.ico":                 []byte("ico"),
	} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := service(dir, repo{}).DetectIcon(context.Background(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != ".repogo/icon.png" || got.Source != ".repogo/icon.png" || len(got.Bytes) != len(large) {
		t.Fatalf("icon = %s (%d bytes), want .repogo/icon.png with its bytes", got.Path, len(got.Bytes))
	}
}

// A web manifest's own pick ranks below the apple touch icon and above the
// generic names, and its largest raster icon wins.
func TestDetectIconRanksTheManifestsLargestIcon(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "public"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"public/manifest.json": `{"icons":[{"src":"/small.png","sizes":"48x48"},{"src":"/big.png?v=2","sizes":"96x96 512x512"},{"src":"/logo.svg"}]}`,
		"public/small.png":     "s",
		"public/big.png":       "b",
		"icon.png":             "i",
	} {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := service(dir, repo{}).DetectIcon(context.Background(), dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != "public/big.png" || got.Source != "manifest.json→big.png" {
		t.Fatalf("icon = %s from %s, want public/big.png from the manifest", got.Path, got.Source)
	}

	if err := os.WriteFile(filepath.Join(dir, "apple-touch-icon.png"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := service(dir, repo{}).DetectIcon(context.Background(), dir, ""); got.Path != "apple-touch-icon.png" {
		t.Fatalf("icon = %s, want the apple touch icon over the manifest", got.Path)
	}
}

// A repository that cannot be read is an error, not a project with none.
func TestDetectIconReportsARepoReadFailure(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service(dir, repo{err: errors.New("cache is gone")}).DetectIcon(context.Background(), dir, ""); err == nil {
		t.Fatal("the repo error was dropped")
	}
}
