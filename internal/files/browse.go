package files

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/repogo/host/internal/errkind"
)

// The pickers reach past the roots, but only to names under home: picking a
// folder is what makes it a root.

var ErrNotBrowsable = errkind.New(errkind.Denied, "files: the folder picker cannot open this folder")

// BrowseOptions narrows a Browse.
type BrowseOptions struct {
	// DirsOnly is the folder picker: folders alone.
	DirsOnly bool
	// Hidden shows, past the roots, the dot entries and ~/Library otherwise
	// left out: the env source picker's .env files are dotfiles.
	Hidden bool
}

// Browse lists one folder, home when path is empty, folders first, and answers
// the resolved folder it listed. Inside a root that is List; past the roots it
// reaches only under home, names and sizes, never contents.
func (s *Service) Browse(path string, opts BrowseOptions) (string, []Entry, error) {
	if path == "" {
		path = s.cfg.Home
	}
	dir, contained, err := s.reach(path, opts.Hidden)
	if err != nil {
		return "", nil, err
	}
	if contained && !opts.DirsOnly {
		entries, err := s.List(dir)
		return dir, entries, err
	}
	home := s.home()
	items, err := os.ReadDir(dir)
	if err != nil {
		return "", nil, wrap(err)
	}
	out := []Entry{}
	for _, item := range items {
		if len(out) >= MaxEntries {
			break
		}
		full := filepath.Join(dir, item.Name())
		if !contained && !shown(home, full, opts.Hidden) {
			continue
		}
		// Stat, not the dirent: a link to a folder opens like one, and reach
		// checks where it lands when it is opened.
		info, err := os.Stat(full)
		if err != nil || (opts.DirsOnly && !info.IsDir()) {
			continue
		}
		entry := Entry{Name: item.Name(), Path: full, Dir: info.IsDir(), ModTime: info.ModTime().UnixMilli()}
		if !entry.Dir {
			entry.Size = info.Size()
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Dir != out[j].Dir {
			return out[i].Dir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return dir, out, nil
}

// Pickable resolves a folder the picker may make a project: any one Browse can
// show, home itself included.
func (s *Service) Pickable(path string) (string, error) {
	dir, _, err := s.reach(path, false)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(dir); err != nil {
		return "", wrap(err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("%w: %s is not a folder", ErrInvalidOperation, path)
	}
	return dir, nil
}

// MakeFolder creates an empty folder in one Browse can show and answers its
// resolved path. The caller makes it a project.
func (s *Service) MakeFolder(parent, name string) (string, error) {
	// A leading dot would make a folder browsable refuses.
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, "/\x00") {
		return "", fmt.Errorf("%w: %q is not a folder name", ErrInvalidOperation, name)
	}
	dir, _, err := s.reach(parent, false)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name)
	if err := os.Mkdir(path, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("%w: %s already exists", ErrInvalidOperation, name)
		}
		return "", wrap(err)
	}
	return path, nil
}

// NewProject makes the folder name a project: under ~/RepoGo when parent is
// empty, otherwise in a folder the picker can show, which it then picks.
func (s *Service) NewProject(parent, name string) (string, error) {
	if parent == "" {
		return s.cfg.Folders.NewFolder(name)
	}
	path, err := s.MakeFolder(parent, name)
	if err != nil {
		return "", err
	}
	return path, s.cfg.Picks.Pick(path, time.Now())
}

// AddProject is the picker's Select: the one call that turns a folder it could
// only see into a root that chats, files, git and terminals may use.
func (s *Service) AddProject(path string) (string, error) {
	dir, err := s.Pickable(path)
	if err != nil {
		return "", err
	}
	return dir, s.cfg.Picks.Pick(dir, time.Now())
}

// reach resolves a folder inside a root, or failing that one browsable shows,
// and reports which it was.
func (s *Service) reach(path string, withHidden bool) (string, bool, error) {
	if dir, err := s.contain(path, false); err == nil {
		return dir, true, nil
	}
	dir, err := s.browsable(path, withHidden)
	return dir, false, err
}

// browsable is the pickers' reach outside the roots: under home, resolved
// first, and never into what shown refuses.
func (s *Service) browsable(path string, withHidden bool) (string, error) {
	home := s.home()
	if home == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: %s", ErrNotBrowsable, path)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", wrap(err)
	}
	if !shown(home, resolved, withHidden) {
		return "", fmt.Errorf("%w: %s", ErrNotBrowsable, path)
	}
	return resolved, nil
}

// home is the picker's home resolved, or empty when it has none.
func (s *Service) home() string {
	if s.cfg.Home == "" {
		return ""
	}
	home, err := filepath.EvalSymlinks(s.cfg.Home)
	if err != nil {
		return ""
	}
	return home
}

// shown is whether a picker may show a resolved path: under home, and unless
// withHidden, not hidden.
func shown(home, path string, withHidden bool) bool {
	if withHidden {
		return home != "" && Within(home, path)
	}
	return !hidden(home, path)
}

// hidden is a resolved path the picker may not show: outside the resolved
// home, or through a hidden folder or ~/Library, where credentials live.
func hidden(home, path string) bool {
	if home == "" || !Within(home, path) {
		return true
	}
	rel, err := filepath.Rel(home, path)
	if err != nil {
		return true
	}
	if rel == "." {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if parts[0] == "Library" {
		return true
	}
	for _, part := range parts {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	return false
}
