package files

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type rootedPath struct {
	root  *os.Root
	name  string
	full  string
	limit int64
}

func (s *Service) directory(path string, scope Scope) (*os.Root, string, error) {
	p, err := s.filePath(path, scope, true)
	if err != nil {
		return nil, "", err
	}
	// The project folder itself: the open root already is it.
	if p.name == "." {
		return p.root, p.full, nil
	}
	defer p.root.Close()
	root, err := p.root.OpenRoot(p.name)
	return root, p.full, wrap(err)
}

// Openat preserves O_NOFOLLOW on the leaf; os.Root follows relative symlinks.
func (p rootedPath) open() (*os.File, error) {
	dir, err := p.root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), p.name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: p.full, Err: err}
	}
	return os.NewFile(uintptr(fd), p.full), nil
}

// filePath pins the checked parent so later path replacements cannot redirect I/O.
func (s *Service) filePath(path string, scope Scope, write bool) (rootedPath, error) {
	var full, base string
	var limit int64
	var err error
	if write {
		roots, rootsErr := s.resolvedRoots()
		if rootsErr != nil {
			return rootedPath{}, rootsErr
		}
		full, base, err = resolveIn(roots, path, true)
	} else {
		full, base, limit, err = s.containRead(path)
	}
	if err != nil {
		return rootedPath{}, err
	}
	if err := scope.check(full); err != nil {
		return rootedPath{}, err
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return rootedPath{}, wrap(err)
	}
	keep := false
	defer func() {
		if !keep {
			root.Close()
		}
	}()
	if scope.Within != "" {
		within, err := filepath.EvalSymlinks(scope.Within)
		if err != nil || !Within(within, full) {
			return rootedPath{}, ErrOutsideScope
		}
		if Within(base, within) && within != base {
			rel, err := filepath.Rel(base, within)
			if err != nil {
				return rootedPath{}, err
			}
			narrow, err := root.OpenRoot(rel)
			if err != nil {
				return rootedPath{}, wrap(err)
			}
			root.Close()
			root, base = narrow, within
		}
	}
	rel, err := filepath.Rel(base, full)
	if err != nil {
		return rootedPath{}, err
	}
	parent := root
	if dir := filepath.Dir(rel); dir != "." {
		if parent, err = root.OpenRoot(dir); err != nil {
			return rootedPath{}, wrap(err)
		}
	} else {
		keep = true
	}
	return rootedPath{root: parent, name: filepath.Base(rel), full: full, limit: limit}, nil
}
