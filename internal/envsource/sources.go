// Package envsource is the secrets a repo's environment.json names by handle
// (`"envFrom": ["my-secrets"]`): each handle bound to a dotenv file on the
// machine that has it, read when a paired device asks, and brokered to a
// host starting services that need it. Values live in memory and in the
// child process environment only; nothing here writes or logs one.
package envsource

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/files"
)

var (
	ErrInvalid     = errkind.New(errkind.Invalid, "invalid env source")
	ErrNotFound    = errkind.New(errkind.NotFound, "no such env source")
	ErrOutsideHome = errkind.New(errkind.Denied, "env source: path is outside the home directory")
)

// MaxBytes bounds a dotenv file; anything bigger is not one.
const MaxBytes = 256 << 10

// Why a handle could not be read, as EnvReadResult.error.
const (
	NotBound   = "not_bound"
	Missing    = "missing"
	Unreadable = "unreadable"
	Binary     = "binary"
	TooLarge   = "too_large"
)

// Source is one binding as a device sees it.
type Source struct {
	Handle string `json:"handle"`
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
}

// ReadResult is one handle's variables, or why there are none.
type ReadResult struct {
	OK    bool              `json:"ok"`
	Vars  map[string]string `json:"vars"`
	Error string            `json:"error,omitempty"`
}

type binding struct {
	Path string `json:"path"`
}

type Sources struct {
	path string
	home string

	mu    sync.Mutex
	bound map[string]binding
}

// Open loads path; home is the directory bindings and browsing are confined to.
func Open(path, home string) (*Sources, error) {
	// Resolved once, so a path is compared with its symlinks already followed.
	home, err := filepath.EvalSymlinks(home)
	if err != nil {
		return nil, err
	}
	s := &Sources{path: path, home: home, bound: map[string]binding{}}
	if _, err := apphome.ReadJSON(path, &s.bound); err != nil {
		return nil, err
	}
	return s, nil
}

// NormalizeHandle is a lowercase slug: letters, digits and single dashes.
func NormalizeHandle(raw string) string {
	var out strings.Builder
	for _, ch := range strings.ToLower(raw) {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') {
			out.WriteRune(ch)
		} else {
			out.WriteByte('-')
		}
	}
	slug := out.String()
	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	return strings.Trim(slug, "-")
}

// List is every binding, by handle.
func (s *Sources) List() []Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Source, 0, len(s.bound))
	for handle, b := range s.bound {
		info, err := os.Stat(b.Path)
		out = append(out, Source{Handle: handle, Path: b.Path, Exists: err == nil && info.Mode().IsRegular()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Handle < out[j].Handle })
	return out
}

// Set binds handle to a dotenv file under home, replacing any earlier binding.
func (s *Sources) Set(handle, path string) (Source, error) {
	handle = NormalizeHandle(handle)
	if handle == "" {
		return Source{}, ErrInvalid.Errorf("a handle is required")
	}
	if !filepath.IsAbs(path) {
		return Source{}, ErrInvalid.Errorf("path must be absolute")
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil && !s.underHome(resolved) {
		return Source{}, fmt.Errorf("%w: %s", ErrOutsideHome, path)
	}
	if _, code := s.load(path); code != "" {
		return Source{}, ErrInvalid.Errorf("%s: %s", path, code)
	}
	s.mu.Lock()
	s.bound[handle] = binding{Path: path}
	err := s.commitLocked()
	s.mu.Unlock()
	if err != nil {
		return Source{}, err
	}
	return Source{Handle: handle, Path: path, Exists: true}, nil
}

func (s *Sources) Remove(handle string) error {
	handle = NormalizeHandle(handle)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.bound[handle]; !ok {
		return ErrNotFound
	}
	delete(s.bound, handle)
	return s.commitLocked()
}

// Read parses each handle's file now, keyed by the handle as asked.
func (s *Sources) Read(handles []string) map[string]ReadResult {
	s.mu.Lock()
	paths := make(map[string]string, len(handles))
	for _, handle := range handles {
		if b, ok := s.bound[NormalizeHandle(handle)]; ok {
			paths[handle] = b.Path
		}
	}
	s.mu.Unlock()

	out := make(map[string]ReadResult, len(handles))
	for _, handle := range handles {
		path, ok := paths[handle]
		if !ok {
			out[handle] = ReadResult{Vars: map[string]string{}, Error: NotBound}
			continue
		}
		b, code := s.load(path)
		if code != "" {
			out[handle] = ReadResult{Vars: map[string]string{}, Error: code}
			continue
		}
		out[handle] = ReadResult{OK: true, Vars: Parse(string(b))}
	}
	return out
}

// load reads a bound file, re-checking everything Set checked: the file may
// have changed, or become a link to somewhere else, since it was bound.
func (s *Sources) load(path string) ([]byte, string) {
	resolved, err := filepath.EvalSymlinks(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, Missing
	}
	if err != nil || !s.underHome(resolved) {
		return nil, Unreadable
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return nil, Unreadable
	}
	if info.Size() > MaxBytes {
		return nil, TooLarge
	}
	b, err := os.ReadFile(resolved)
	if err != nil {
		return nil, Unreadable
	}
	if len(b) > MaxBytes {
		return nil, TooLarge
	}
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return nil, Binary
	}
	return b, ""
}

func (s *Sources) underHome(path string) bool {
	return files.Within(s.home, path)
}

// commitLocked writes the bindings. 0600: a path to a secrets file says where
// the secrets are.
func (s *Sources) commitLocked() error {
	return apphome.WriteJSON(s.path, s.bound, 0o600)
}
