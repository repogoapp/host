package files

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/repogo/host/internal/errkind"
)

var (
	ErrOutsideScope = errkind.New(errkind.Denied, "files: path is outside what this call may touch")
	ErrChanged      = errkind.New(errkind.Invalid, "files: the file changed since it was read")
)

// Scope narrows a call to one folder and to files with the given extensions
// (any case), checked on the resolved path so a link cannot lead out or to
// another kind of file. The zero Scope narrows nothing.
type Scope struct {
	Within     string   `json:"within"`
	Extensions []string `json:"extensions"`
}

func (sc Scope) narrows() bool { return sc.Within != "" || len(sc.Extensions) > 0 }

func (sc Scope) check(full string) error {
	if len(sc.Extensions) > 0 && !hasExtension(full, sc.Extensions) {
		return fmt.Errorf("%w: %s is not a %s file", ErrOutsideScope, filepath.Base(full), strings.Join(sc.Extensions, " or "))
	}
	if sc.Within == "" {
		return nil
	}
	within, err := filepath.EvalSymlinks(filepath.Clean(sc.Within))
	if err != nil || !Within(within, full) {
		return fmt.Errorf("%w: %s", ErrOutsideScope, full)
	}
	return nil
}

func hasExtension(name string, extensions []string) bool {
	ext := strings.TrimPrefix(filepath.Ext(name), ".")
	for _, want := range extensions {
		if ext != "" && strings.EqualFold(ext, strings.TrimPrefix(want, ".")) {
			return true
		}
	}
	return false
}

// WriteOptions is how a write may land. ModTime, when set, is the mod_time
// the caller last read: a file changed or removed since is refused rather
// than overwritten.
type WriteOptions struct {
	Create  bool
	ModTime int64
	Scope   Scope
}

// ListFiles is every file under path with one of extensions, Name relative to
// path, walked as Search walks (same skips, no links, same bound). A non-empty
// within holds the resolved path to that folder, so a link cannot leave it.
func (s *Service) ListFiles(path string, extensions []string, within string) (entries []Entry, truncated bool, err error) {
	if len(extensions) == 0 {
		return nil, false, fmt.Errorf("%w: no extensions", ErrInvalidOperation)
	}
	root, dir, err := s.directory(path, Scope{Within: within})
	if err != nil {
		return nil, false, wrap(err)
	}
	defer root.Close()

	entries = []Entry{}
	seen := 0
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		seen++
		if seen > maxSearchEntries || len(entries) >= MaxEntries {
			truncated = true
			return fs.SkipAll
		}
		if entry.IsDir() {
			if skippedDirs[entry.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() || !hasExtension(name, extensions) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil // vanished mid-walk
		}
		entries = append(entries, Entry{
			Name: filepath.ToSlash(name), Path: filepath.Join(dir, name),
			Size: info.Size(), ModTime: info.ModTime().UnixMilli(),
		})
		return nil
	})
	sort.SliceStable(entries, func(i, j int) bool { return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name) })
	return entries, truncated, wrap(err)
}

// ReadIn is Read held to scope.
func (s *Service) ReadIn(path string, scope Scope) (File, error) {
	file, _, err := s.read(path, scope, 0)
	return file, err
}

// ReadChanged skips content when the opened file still has the caller's mod time.
func (s *Service) ReadChanged(path string, scope Scope, modTime int64) (File, bool, error) {
	return s.read(path, scope, modTime)
}

// WriteWith is Write or Create with opts' scope and changed-file check.
func (s *Service) WriteWith(path string, content []byte, opts WriteOptions) (File, error) {
	return s.write(path, content, opts)
}

// Replace swaps the one place old appears in a text file for replacement. Old must be
// there exactly once, so an edit never lands somewhere the caller did not mean.
func (s *Service) Replace(path, old, replacement string, opts WriteOptions) (File, error) {
	if old == "" {
		return File{}, fmt.Errorf("%w: the text to replace is empty", ErrInvalidOperation)
	}
	file, err := s.ReadIn(path, opts.Scope)
	if err != nil {
		return File{}, err
	}
	if opts.ModTime != 0 && file.ModTime != opts.ModTime {
		return File{}, fmt.Errorf("%w: %s", ErrChanged, path)
	}
	if file.Binary || !utf8.Valid(file.Content) {
		return File{}, fmt.Errorf("%w: %s is not a text file", ErrInvalidOperation, path)
	}
	content := string(file.Content)
	switch n := occurrences(content, old); n {
	case 0:
		return File{}, fmt.Errorf("%w: the text to replace is not in %s", ErrInvalidOperation, filepath.Base(path))
	case 1:
	default:
		return File{}, fmt.Errorf("%w: the text to replace appears %d times in %s", ErrInvalidOperation, n, filepath.Base(path))
	}
	opts.Create = false
	opts.ModTime = file.ModTime
	return s.write(file.Path, []byte(strings.Replace(content, old, replacement, 1)), opts)
}

// occurrences counts every place old starts in content, overlapping ones
// included: "aa" is in "aaa" twice, so replacing "the one place" is ambiguous.
func occurrences(content, old string) int {
	n := 0
	for i := 0; i+len(old) <= len(content); {
		j := strings.Index(content[i:], old)
		if j < 0 {
			break
		}
		n++
		i += j + 1
	}
	return n
}
