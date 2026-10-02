package files

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/repogo/host/internal/errkind"
)

var ErrInvalidOperation = errkind.New(errkind.Invalid, "files: invalid operation")

// entry keeps a final symlink intact so deleting it never deletes its target.
func entry(roots []string, path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", ErrOutsideRoots
	}
	parent, err := containIn(roots, filepath.Dir(filepath.Clean(path)), false)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(filepath.Clean(path))), nil
}

// mutationRoot opens the root holding path, refusing a root itself.
func mutationRoot(roots []string, path string) (*os.Root, string, error) {
	for _, root := range roots {
		if Within(path, root) {
			return nil, "", fmt.Errorf("%w: cannot move or remove a project root", ErrInvalidOperation)
		}
	}
	for _, root := range roots {
		if Within(root, path) {
			handle, err := os.OpenRoot(root)
			if err != nil {
				return nil, "", wrap(err)
			}
			relative, err := filepath.Rel(root, path)
			if err != nil {
				handle.Close()
				return nil, "", err
			}
			return handle, relative, nil
		}
	}
	return nil, "", ErrOutsideRoots
}

func (s *Service) Delete(path string) error {
	roots, err := s.resolvedRoots()
	if err != nil {
		return err
	}
	full, err := entry(roots, path)
	if err != nil {
		return err
	}
	root, relative, err := mutationRoot(roots, full)
	if err != nil {
		return err
	}
	defer root.Close()
	if _, err := root.Lstat(relative); err != nil {
		return wrap(err)
	}
	return wrap(root.RemoveAll(relative))
}

func (s *Service) Rename(path, newPath string) error {
	roots, err := s.resolvedRoots()
	if err != nil {
		return err
	}
	full, err := entry(roots, path)
	if err != nil {
		return err
	}
	destination, err := entry(roots, newPath)
	if err != nil {
		return err
	}
	root, relative, err := mutationRoot(roots, full)
	if err != nil {
		return err
	}
	defer root.Close()
	targetRoot, _, err := mutationRoot(roots, destination)
	if err != nil {
		return err
	}
	targetRoot.Close()
	// One rooted rename cannot cross project boundaries.
	if !Within(root.Name(), destination) {
		return ErrOutsideRoots
	}
	target, err := filepath.Rel(root.Name(), destination)
	if err != nil {
		return err
	}
	if _, err := root.Lstat(target); err == nil {
		return fmt.Errorf("%w: destination exists", ErrInvalidOperation)
	} else if !os.IsNotExist(err) {
		return wrap(err)
	}
	return wrap(root.Rename(relative, target))
}

func (s *Service) Mkdir(path string) error {
	roots, err := s.resolvedRoots()
	if err != nil {
		return err
	}
	full, err := entry(roots, path)
	if err != nil {
		return err
	}
	root, relative, err := mutationRoot(roots, full)
	if err != nil {
		return err
	}
	defer root.Close()
	return wrap(root.Mkdir(relative, 0o755))
}
