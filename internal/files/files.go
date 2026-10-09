// Package files reads and writes the user's project files. Every path is
// contained to a root the host already knows about: pairing authorizes a
// device, it does not grant an unrestricted write to `~/.zshrc`.
package files

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/repogo/host/internal/errkind"
)

const (
	// MaxFileBytes bounds a read. Source files are kilobytes; something far
	// past this is a build artifact or a database, and the honest answer is an
	// error rather than a multi-megabyte round trip to a phone.
	MaxFileBytes = 2 << 20

	// MaxEntries bounds one directory listing. `node_modules` has tens of
	// thousands of entries and nobody scrolls them.
	MaxEntries = 2000
)

var (
	ErrOutsideRoots = errkind.New(errkind.Denied, "files: path is outside every known project")
	ErrNotFound     = errkind.New(errkind.NotFound, "files: no such file or directory")
	ErrTooLarge     = errkind.New(errkind.Invalid, "files: file is larger than the read limit")
	ErrIsDirectory  = errkind.New(errkind.Invalid, "files: path is a directory")
	ErrNoRoots      = errkind.New(errkind.Denied, "files: this host has no known projects yet")
)

// Roots is where the allowed directories come from. Host state, never a
// parameter: a caller that can name its own root has no restriction at all.
type Roots interface {
	Roots() ([]string, error)
}

// StaticRoots is a fixed set of roots.
type StaticRoots []string

func (r StaticRoots) Roots() ([]string, error) { return r, nil }

// Picker keeps a folder the user picked as a root, most recent first.
type Picker interface {
	Pick(path string, at time.Time) error
}

// FolderMaker makes a new top-level project, which no root contains.
type FolderMaker interface {
	NewFolder(name string) (string, error)
}

// Container is the path guard every family that touches a project shares.
type Container interface {
	Contain(path string) (string, error)
}

// Union serves the roots of several sources, in order, without duplicates. A
// source that fails is skipped: losing the chat cache should cost its
// projects, not every project.
func Union(sources ...Roots) Roots { return union(sources) }

type union []Roots

func (u union) Roots() ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, source := range u {
		if source == nil {
			continue
		}
		roots, err := source.Roots()
		if err != nil {
			continue
		}
		for _, root := range roots {
			if root == "" || seen[root] {
				continue
			}
			seen[root] = true
			out = append(out, root)
		}
	}
	return out, nil
}

type Entry struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Dir     bool   `json:"dir"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
}

type File struct {
	Path    string `json:"path"`
	Content []byte `json:"content" wire:"array"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`

	// Binary means the content is not text. The caller gets the bytes anyway —
	// it may be an image — but must not try to render them in an editor.
	Binary bool `json:"binary"`
}

// Config is what a Service serves. Picks and Folders are used only by
// NewProject and AddProject.
type Config struct {
	Roots Roots

	// Home is where the folder picker browses outside the roots; empty, it
	// cannot. See browse.go.
	Home string

	Picks   Picker
	Folders FolderMaker

	// ReadOnly is host state Read, and nothing else, reaches: the files sent
	// with a prompt live outside every project, and the phone draws them back.
	ReadOnly []ReadOnlyDir
}

// ReadOnlyDir is a directory a phone may see but not change, with its own read limit.
type ReadOnlyDir struct {
	Path     string
	MaxBytes int64
}

type Service struct {
	cfg Config
}

func New(c Config) *Service { return &Service{cfg: c} }

// Projects lists the roots this host will serve, so a client can offer them
// rather than guessing at paths it is not allowed to reach.
func (s *Service) Projects() ([]Entry, error) {
	roots, err := s.resolvedRoots()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(roots))
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			// A project whose folder was moved or deleted since its last chat.
			// Skipped rather than failing the list: the others are still valid.
			continue
		}
		out = append(out, Entry{
			Name: filepath.Base(root), Path: root, Dir: true,
			ModTime: info.ModTime().UnixMilli(),
		})
	}
	// Deliberately NOT re-sorted: Roots arrives in recency order, which is the
	// only useful ordering for a list this long.
	return out, nil
}

// Contain resolves a path and proves it lives inside a known project. Exported
// so every family shares one containment implementation.
func (s *Service) Contain(path string) (string, error) { return s.contain(path, false) }

func (s *Service) List(path string) ([]Entry, error) {
	root, dir, err := s.directory(path, Scope{})
	if err != nil {
		return nil, err
	}
	defer root.Close()
	items, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, wrap(err)
	}

	out := make([]Entry, 0, min(len(items), MaxEntries))
	for _, item := range items {
		if len(out) >= MaxEntries {
			break
		}
		// Not item.Info: on Go 1.25 a nested root's entries stat "./name" and fail.
		info, err := root.Lstat(item.Name())
		if err != nil {
			continue // vanished between the read and the stat; not worth failing over
		}
		out = append(out, Entry{
			Name: item.Name(), Path: filepath.Join(dir, item.Name()),
			Dir: item.IsDir(), Size: info.Size(), ModTime: info.ModTime().UnixMilli(),
		})
	}
	// Directories first, then by name — the order a file tree is read in.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func (s *Service) Read(path string) (File, error) {
	file, _, err := s.read(path, Scope{}, 0)
	return file, err
}

func (s *Service) read(path string, scope Scope, modTime int64) (File, bool, error) {
	p, err := s.filePath(path, scope, false)
	if err != nil {
		return File{}, false, err
	}
	defer p.root.Close()
	// Resolution already followed the final link; a replacement must not redirect the read.
	f, err := p.open()
	if err != nil {
		return File{}, false, wrap(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return File{}, false, wrap(err)
	}
	if info.IsDir() {
		return File{}, false, fmt.Errorf("%w: %s", ErrIsDirectory, path)
	}
	if !info.Mode().IsRegular() {
		return File{}, false, fmt.Errorf("%w: not a regular file", ErrInvalidOperation)
	}
	file := File{Path: p.full, Size: info.Size(), ModTime: info.ModTime().UnixMilli()}
	if modTime != 0 && file.ModTime == modTime {
		return file, false, nil
	}
	if info.Size() > p.limit {
		return File{}, false, ErrTooLarge
	}
	content, err := io.ReadAll(io.LimitReader(f, p.limit+1))
	if err != nil {
		return File{}, false, wrap(err)
	}
	if int64(len(content)) > p.limit {
		return File{}, false, ErrTooLarge
	}
	file.Content, file.Binary = content, isBinary(content)
	return file, true, nil
}

// Write replaces a file's contents through a temp file and rename, so a running
// agent never sees a half-written file. The parent must already exist.
func (s *Service) Write(path string, content []byte) (File, error) {
	return s.write(path, content, WriteOptions{})
}

// Create is Write for a file that must not exist yet: the temp file is linked
// into place, which fails rather than replace a file that appeared meanwhile.
func (s *Service) Create(path string, content []byte) (File, error) {
	return s.write(path, content, WriteOptions{Create: true})
}

func (s *Service) write(path string, content []byte, opts WriteOptions) (File, error) {
	create := opts.Create
	// A scoped caller is held to the read limit too; the Files tab is not,
	// since it uploads what the user picked.
	if opts.Scope.narrows() && len(content) > MaxFileBytes {
		return File{}, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(content))
	}
	p, err := s.filePath(path, opts.Scope, true)
	if err != nil {
		return File{}, err
	}
	defer p.root.Close()
	info, err := p.root.Lstat(p.name)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return File{}, wrap(err)
	}
	mode := os.FileMode(0o644)
	if err == nil {
		if create {
			return File{}, fmt.Errorf("%w: %s already exists", ErrInvalidOperation, path)
		}
		if info.IsDir() {
			return File{}, fmt.Errorf("%w: %s", ErrIsDirectory, path)
		}
		if !info.Mode().IsRegular() {
			return File{}, fmt.Errorf("%w: not a regular file", ErrInvalidOperation)
		}
		mode = info.Mode().Perm()
	}

	// Both names stay relative to the pinned parent even if its path is replaced.
	tmpName := ".repogo-" + rand.Text()
	tmp, err := p.root.OpenFile(tmpName, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return File{}, wrap(err)
	}
	defer p.root.Remove(tmpName)
	defer tmp.Close()
	if _, err := tmp.Write(content); err != nil {
		return File{}, wrap(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		return File{}, wrap(err)
	}
	info, err = tmp.Stat()
	if err != nil {
		return File{}, wrap(err)
	}
	if err := tmp.Close(); err != nil {
		return File{}, wrap(err)
	}
	// A stale editor must not overwrite a newer save.
	if !create && opts.ModTime != 0 {
		current, err := p.root.Lstat(p.name)
		if err != nil || !current.Mode().IsRegular() || current.ModTime().UnixMilli() != opts.ModTime {
			return File{}, fmt.Errorf("%w: %s", ErrChanged, path)
		}
	}
	if create {
		if err := p.root.Link(tmpName, p.name); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return File{}, fmt.Errorf("%w: %s already exists", ErrInvalidOperation, path)
			}
			return File{}, wrap(err)
		}
	} else if err := p.root.Rename(tmpName, p.name); err != nil {
		return File{}, wrap(err)
	}
	return File{Path: p.full, Size: info.Size(), ModTime: info.ModTime().UnixMilli()}, nil
}

// contain resolves symlinks before the check, so `project/link` pointing at
// `~/.ssh` fails. With `allowMissing` the deepest existing ancestor is
// resolved instead.
func (s *Service) contain(path string, allowMissing bool) (string, error) {
	roots, err := s.resolvedRoots()
	if err != nil {
		return "", err
	}
	return containIn(roots, path, allowMissing)
}

// containIn is contain against roots already resolved, for a call that
// contains more than one path.
func containIn(roots []string, path string, allowMissing bool) (string, error) {
	full, _, err := resolveIn(roots, path, allowMissing)
	return full, err
}

func resolveIn(roots []string, path string, allowMissing bool) (string, string, error) {
	if path == "" {
		return "", "", fmt.Errorf("%w: empty path", ErrOutsideRoots)
	}
	if len(roots) == 0 {
		return "", "", ErrNoRoots
	}

	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return "", "", fmt.Errorf("%w: %s is not an absolute path", ErrOutsideRoots, path)
	}

	resolved, err := resolveExisting(clean, allowMissing)
	if err != nil {
		return "", "", err
	}
	for _, root := range roots {
		if Within(root, resolved) {
			return resolved, root, nil
		}
	}
	return "", "", fmt.Errorf("%w: %s", ErrOutsideRoots, path)
}

// containRead is contain widened by the ReadOnly directories, checked first so
// a host with no projects still shows what it was sent. Symlinks resolve before
// the check, so a link in there to `~/.ssh` falls through and is refused.
func (s *Service) containRead(path string) (string, string, int64, error) {
	if clean := filepath.Clean(path); path != "" && filepath.IsAbs(clean) {
		if resolved, err := filepath.EvalSymlinks(clean); err == nil {
			for _, dir := range s.cfg.ReadOnly {
				root, err := filepath.EvalSymlinks(dir.Path)
				if err == nil && Within(root, resolved) {
					return resolved, root, dir.MaxBytes, nil
				}
			}
		}
	}
	roots, err := s.resolvedRoots()
	if err != nil {
		return "", "", 0, err
	}
	full, base, err := resolveIn(roots, path, false)
	return full, base, MaxFileBytes, err
}

// resolveExisting follows symlinks on the deepest part of the path that exists.
func resolveExisting(path string, allowMissing bool) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return "", wrap(err)
	}
	if !allowMissing {
		return "", fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	// A new file: the parent is what must be real and contained.
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", wrap(err)
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

// Within reports whether path is root or inside it. String comparison only:
// both sides must already be resolved, which is the caller's job.
func Within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	// `..` at the front of the relative path means it climbed out.
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *Service) resolvedRoots() ([]string, error) {
	raw, err := s.cfg.Roots.Roots()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(raw))
	for _, root := range raw {
		if root == "" {
			continue
		}
		// Roots are resolved too: comparing a resolved path against an
		// unresolved root fails for every user whose projects live under a
		// symlink, which on a Mac includes anything in /tmp.
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		out = append(out, resolved)
	}
	return out, nil
}

// isBinary looks for a NUL in the first few KB, as editors and git do; the
// caller only wants to know whether rendering as text produces garbage.
func isBinary(content []byte) bool {
	head := content
	if len(head) > 8000 {
		head = head[:8000]
	}
	for _, b := range head {
		if b == 0 {
			return true
		}
	}
	return false
}

// wrap keeps the OS error readable while giving callers a sentinel to match on.
func wrap(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrNotFound, err)
	}
	return err
}
