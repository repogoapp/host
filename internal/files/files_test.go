package files_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/repogo/host/internal/files"
)

// The containment rules are the whole point of this package, so most of what is
// below is an attempt to get OUT of a root.

// project returns a root with a file in it, plus the parent it must not escape
// into. `secret` stands in for anything on the disk that is not a project —
// ~/.ssh, a keychain, another customer's repo.
func project(t *testing.T) (svc *files.Service, root, secret string) {
	t.Helper()
	// EvalSymlinks: on a Mac /var is a symlink to /private/var, so an unresolved
	// TempDir compares unequal to every resolved path under it.
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(base, "project")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	secret = filepath.Join(base, "secrets.txt")
	if err := os.WriteFile(secret, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	return files.New(files.Config{Roots: files.StaticRoots{root}}), root, secret
}

func TestReadsAFileInsideAProject(t *testing.T) {
	svc, root, _ := project(t)

	file, err := svc.Read(filepath.Join(root, "src", "main.go"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(file.Content) != "package main\n" {
		t.Errorf("content = %q", string(file.Content))
	}
	if file.Binary {
		t.Error("a Go source file was reported as binary")
	}
}

func TestListsDirectoriesFirst(t *testing.T) {
	svc, root, _ := project(t)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := svc.List(root)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	// A file tree reads directories first; a client that has to re-sort is a
	// client that will sort differently from the next one.
	if !entries[0].Dir || entries[0].Name != "src" {
		t.Errorf("first entry = %+v, want the directory", entries[0])
	}
}

// --- the escapes -------------------------------------------------------------

func TestRefusesAPathOutsideEveryRoot(t *testing.T) {
	svc, _, secret := project(t)

	if _, err := svc.Read(secret); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("read outside a root: %v, want ErrOutsideRoots", err)
	}
	if _, err := svc.List(filepath.Dir(secret)); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("list outside a root: %v, want ErrOutsideRoots", err)
	}
	if _, err := svc.Write(secret, []byte("owned")); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("write outside a root: %v, want ErrOutsideRoots", err)
	}
	// And the file it was aimed at is untouched.
	content, err := os.ReadFile(secret)
	if err != nil || string(content) != "PRIVATE KEY" {
		t.Errorf("the file outside the root was modified: %q, %v", string(content), err)
	}
}

func TestRefusesTraversalOutOfARoot(t *testing.T) {
	svc, root, _ := project(t)

	escape := filepath.Join(root, "..", "secrets.txt")
	if _, err := svc.Read(escape); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("`..` traversal: %v, want ErrOutsideRoots", err)
	}
}

// A symlink is the case a string comparison cannot catch: the path is textually
// inside the project and points anywhere the user's account can reach.
func TestRefusesASymlinkPointingOutOfARoot(t *testing.T) {
	svc, root, secret := project(t)

	link := filepath.Join(root, "innocent.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := svc.Read(link); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("symlink out of a root: %v, want ErrOutsideRoots", err)
	}
	if _, err := svc.Write(link, []byte("owned")); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("write through a symlink: %v, want ErrOutsideRoots", err)
	}
	content, _ := os.ReadFile(secret)
	if string(content) != "PRIVATE KEY" {
		t.Error("a write followed a symlink out of the project")
	}
}

// Writing a NEW file resolves the parent instead, and the parent has to be
// contained too — otherwise `project/../evil.sh` is creatable.
func TestNewFileIsCheckedByItsParent(t *testing.T) {
	svc, root, secret := project(t)

	if _, err := svc.Write(filepath.Join(root, "src", "new.go"), []byte("package main\n")); err != nil {
		t.Fatalf("write a new file inside the project: %v", err)
	}
	outside := filepath.Join(filepath.Dir(secret), "evil.sh")
	if _, err := svc.Write(outside, []byte("rm -rf /")); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("new file outside a root: %v, want ErrOutsideRoots", err)
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("a file was created outside every project")
	}
}

// A host with no chats yet has no roots, and no roots must mean nothing is
// reachable — not that everything is.
func TestNoRootsServesNothing(t *testing.T) {
	svc := files.New(files.Config{Roots: files.StaticRoots{}})

	if _, err := svc.List("/"); !errors.Is(err, files.ErrNoRoots) {
		t.Errorf("list with no roots: %v, want ErrNoRoots", err)
	}
	if _, err := svc.Read("/etc/hosts"); !errors.Is(err, files.ErrNoRoots) {
		t.Errorf("read with no roots: %v, want ErrNoRoots", err)
	}
}

// --- read-only directories ---------------------------------------------------

// sent returns a service that may also read `dir`, the way the host reads the
// files sent with a prompt, with a file already in it.
func sent(t *testing.T, maxBytes int64) (svc *files.Service, root, dir, secret string) {
	t.Helper()
	_, root, secret = project(t)
	dir = filepath.Join(filepath.Dir(root), "attachments")
	if err := os.MkdirAll(filepath.Join(dir, "batch"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "batch", "shot.png"), []byte("png"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc = files.New(files.Config{Roots: files.StaticRoots{root}, ReadOnly: []files.ReadOnlyDir{{Path: dir, MaxBytes: maxBytes}}})
	return svc, root, dir, secret
}

func TestReadsAFileInAReadOnlyDirectory(t *testing.T) {
	svc, _, dir, _ := sent(t, files.MaxFileBytes)

	file, err := svc.Read(filepath.Join(dir, "batch", "shot.png"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(file.Content) != "png" {
		t.Errorf("content = %q", string(file.Content))
	}
}

// Read-only means exactly that: every other operation keeps to the projects.
func TestReadOnlyDirectoryRefusesEverythingButRead(t *testing.T) {
	svc, _, dir, _ := sent(t, files.MaxFileBytes)
	path := filepath.Join(dir, "batch", "shot.png")

	if _, err := svc.Write(path, []byte("owned")); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("write: %v, want ErrOutsideRoots", err)
	}
	if _, err := svc.List(dir); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("list: %v, want ErrOutsideRoots", err)
	}
	if err := svc.Delete(path); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("delete: %v, want ErrOutsideRoots", err)
	}
	if _, err := svc.Contain(path); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("contain: %v, want ErrOutsideRoots", err)
	}
	if content, _ := os.ReadFile(path); string(content) != "png" {
		t.Error("a file in the read-only directory was modified")
	}
}

func TestReadOnlyDirectoryRefusesEscapes(t *testing.T) {
	svc, _, dir, secret := sent(t, files.MaxFileBytes)

	if _, err := svc.Read(filepath.Join(dir, "..", "secrets.txt")); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("`..` traversal: %v, want ErrOutsideRoots", err)
	}
	link := filepath.Join(dir, "batch", "innocent.png")
	if err := os.Symlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := svc.Read(link); !errors.Is(err, files.ErrOutsideRoots) {
		t.Errorf("symlink out of the directory: %v, want ErrOutsideRoots", err)
	}
}

// A host with no projects yet still shows what it was sent.
func TestReadOnlyDirectoryNeedsNoProjects(t *testing.T) {
	_, _, dir, _ := sent(t, files.MaxFileBytes)
	svc := files.New(files.Config{Roots: files.StaticRoots{}, ReadOnly: []files.ReadOnlyDir{{Path: dir, MaxBytes: files.MaxFileBytes}}})

	if _, err := svc.Read(filepath.Join(dir, "batch", "shot.png")); err != nil {
		t.Errorf("read with no projects: %v", err)
	}
	if _, err := svc.Read("/etc/hosts"); !errors.Is(err, files.ErrNoRoots) {
		t.Errorf("read elsewhere with no projects: %v, want ErrNoRoots", err)
	}
}

// Each directory carries its own limit: an attachment may be larger than any
// source file, and a project file must not borrow that allowance.
func TestReadOnlyDirectoryHasItsOwnLimit(t *testing.T) {
	svc, root, dir, _ := sent(t, 2*files.MaxFileBytes)
	big := make([]byte, files.MaxFileBytes+1)
	sentBig := filepath.Join(dir, "batch", "big.png")
	projectBig := filepath.Join(root, "big.bin")
	for _, p := range []string{sentBig, projectBig} {
		if err := os.WriteFile(p, big, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := svc.Read(sentBig); err != nil {
		t.Errorf("attachment under its limit: %v", err)
	}
	if _, err := svc.Read(projectBig); !errors.Is(err, files.ErrTooLarge) {
		t.Errorf("project file over the project limit: %v, want ErrTooLarge", err)
	}
}

// --- ordinary limits ---------------------------------------------------------

func TestWriteIsAtomicAndKeepsTheMode(t *testing.T) {
	svc, root, _ := project(t)
	path := filepath.Join(root, "src", "main.go")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Write(path, []byte("edited\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "edited\n" {
		t.Fatalf("content = %q, %v", string(content), err)
	}
	// The temp file is 0600; inheriting that would show up as a mode change in
	// the user's next `git status`.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want the file's original 0600", info.Mode().Perm())
	}
	// And nothing is left behind from the rename.
	entries, _ := os.ReadDir(filepath.Join(root, "src"))
	if len(entries) != 1 {
		t.Errorf("%d entries left in the directory, want just the file", len(entries))
	}
}

func TestRefusesAFileLargerThanTheLimit(t *testing.T) {
	svc, root, _ := project(t)
	big := filepath.Join(root, "big.bin")
	if err := os.WriteFile(big, make([]byte, files.MaxFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.Read(big); !errors.Is(err, files.ErrTooLarge) {
		t.Errorf("oversized read: %v, want ErrTooLarge", err)
	}
}

func TestReportsBinaryContent(t *testing.T) {
	svc, root, _ := project(t)
	path := filepath.Join(root, "logo.png")
	if err := os.WriteFile(path, []byte{0x89, 'P', 'N', 'G', 0x00, 0x1a}, 0o644); err != nil {
		t.Fatal(err)
	}

	file, err := svc.Read(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Flagged rather than refused: it may be an image the client wants to show.
	// What it must not do is render it into a text editor.
	if !file.Binary {
		t.Error("a file containing NUL was not reported as binary")
	}
}

func TestReadingADirectoryIsAnError(t *testing.T) {
	svc, root, _ := project(t)

	if _, err := svc.Read(filepath.Join(root, "src")); !errors.Is(err, files.ErrIsDirectory) {
		t.Errorf("read a directory: %v, want ErrIsDirectory", err)
	}
}

func TestMissingFileIsNotFound(t *testing.T) {
	svc, root, _ := project(t)

	if _, err := svc.Read(filepath.Join(root, "nope.go")); !errors.Is(err, files.ErrNotFound) {
		t.Errorf("missing file: %v, want ErrNotFound", err)
	}
}

// Union is how a folder becomes a project by more than one route — a chat has
// run there, or it was cloned — without either source knowing about the other.
func TestUnionMergesSourcesAndDropsDuplicates(t *testing.T) {
	got, err := files.Union(files.StaticRoots{"/a", "/b"}, files.StaticRoots{"/b", "/c"}, nil).Roots()
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	want := []string{"/a", "/b", "/c"}
	if len(got) != len(want) {
		t.Fatalf("Roots = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Roots[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// Losing the chat cache should cost you its projects, not every project.
func TestUnionSurvivesAFailingSource(t *testing.T) {
	got, err := files.Union(failingRoots{}, files.StaticRoots{"/kept"}).Roots()
	if err != nil {
		t.Fatalf("Roots: %v", err)
	}
	if len(got) != 1 || got[0] != "/kept" {
		t.Errorf("Roots = %v, want [/kept]", got)
	}
}

type failingRoots struct{}

func (failingRoots) Roots() ([]string, error) { return nil, errors.New("cache is gone") }
