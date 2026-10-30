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

	"github.com/repogoapp/host/internal/errkind"
)

// The pickers reach past the roots, but only to names under home and, for the
// folder picker, on the drives plugged in: picking a folder is what makes it a
// root.

var ErrNotBrowsable = errkind.New(errkind.Denied, "files: the folder picker cannot open this folder")

// BrowseOptions narrows a Browse.
type BrowseOptions struct {
	// DirsOnly is the folder picker: folders alone.
	DirsOnly bool
	// Hidden shows, past the roots, the dot entries and ~/Library otherwise
	// left out: the env source picker's .env files are dotfiles.
	Hidden bool
}

// Browse lists one folder, home when path is empty, folders first, answering the
// folder it resolved. Inside a root it is List; past them, names and sizes only,
// under home or on a drive, which the folder picker lists first in home.
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
	places := s.places(opts.Hidden)
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
		if !contained && !places.shown(full) {
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
	if opts.DirsOnly && !contained && places.volumes != "" && dir == places.home {
		out = append(places.drives(), out...)
	}
	return dir, out, nil
}

// VolumesDir is where macOS mounts drives; other systems mount them where
// their owner chooses, so the picker shows none there.
func VolumesDir(goos string) string {
	if goos != "darwin" {
		return ""
	}
	return "/Volumes"
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

// browsable is the pickers' reach outside the roots: under home or on a
// drive, resolved first, and never into what shown refuses.
func (s *Service) browsable(path string, withHidden bool) (string, error) {
	places := s.places(withHidden)
	if places.home == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: %s", ErrNotBrowsable, path)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return "", wrap(err)
	}
	if !places.shown(resolved) {
		return "", fmt.Errorf("%w: %s", ErrNotBrowsable, path)
	}
	return resolved, nil
}

// places is where a picker may look past the roots, each resolved: home, and
// for the folder picker the folder drives mount in.
type places struct {
	home, volumes string
	// hidden shows dot entries and ~/Library, for the env source picker,
	// which looks only under home.
	hidden bool
}

func (s *Service) places(withHidden bool) places {
	p := places{home: resolve(s.cfg.Home), hidden: withHidden}
	if !withHidden {
		p.volumes = resolve(s.cfg.Volumes)
	}
	return p
}

// resolve is path with its links followed, or empty when it has none.
func resolve(path string) string {
	if path == "" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	return resolved
}

// shown is whether a picker may show a resolved path: under home or on a drive,
// and unless hidden, through no dot folder or ~/Library, where credentials live.
// The startup disk's link in /Volumes resolves to /, out of reach.
func (p places) shown(path string) bool {
	if p.home != "" && Within(p.home, path) {
		if p.hidden {
			return true
		}
		return visible(p.home, path, "Library")
	}
	if p.volumes != "" && path != p.volumes && Within(p.volumes, path) {
		return visible(p.volumes, path, "")
	}
	return false
}

// visible is whether path, inside place, passes through no dot folder and
// does not start at the folder named off.
func visible(place, path, off string) bool {
	rel, err := filepath.Rel(place, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if parts[0] == off {
		return false
	}
	for _, part := range parts {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

// drives are the volumes folder's own folders, the startup disk's link left out.
// No drive is read: a dropped share, or the prompt macOS shows before a program
// reads a removable drive, would hold the listing.
func (p places) drives() []Entry {
	items, err := os.ReadDir(p.volumes)
	if err != nil {
		return nil
	}
	out := []Entry{}
	for _, item := range items {
		if !item.IsDir() || strings.HasPrefix(item.Name(), ".") {
			continue
		}
		out = append(out, Entry{Name: item.Name(), Path: filepath.Join(p.volumes, item.Name()), Dir: true})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out
}
