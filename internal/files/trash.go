package files

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"
)

// TrashDir is the folder macOS keeps deleted files in; other systems have none
// a program can count on, so what's deleted there is gone for good.
func TrashDir(goos, home string) string {
	if goos != "darwin" || home == "" {
		return ""
	}
	return filepath.Join(home, ".Trash")
}

// DeleteProject moves a project's folder to the Trash, or deletes it where
// there is none. Only a root itself, never home, the projects folder or a
// folder above either.
func (s *Service) DeleteProject(path string) error {
	if !filepath.IsAbs(path) {
		return ErrOutsideRoots
	}
	dir, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		return wrap(err)
	}
	roots, err := s.resolvedRoots()
	if err != nil {
		return err
	}
	if !slices.Contains(roots, dir) {
		return fmt.Errorf("%w: %s is not a project", ErrInvalidOperation, path)
	}
	for _, kept := range []string{s.cfg.Home, s.cfg.Projects, s.cfg.Trash} {
		if kept == "" {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(kept); err == nil {
			kept = resolved
		}
		if Within(dir, kept) {
			return fmt.Errorf("%w: %s holds every project and can't be deleted", ErrInvalidOperation, path)
		}
	}
	if s.cfg.Trash == "" {
		return wrap(os.RemoveAll(dir))
	}
	return trash(dir, s.cfg.Trash, time.Now())
}

// trash names the item by when it went, as Finder does, so a second one of
// the same name never lands on the first.
func trash(dir, bin string, at time.Time) error {
	if err := os.MkdirAll(bin, 0o700); err != nil {
		return err
	}
	name := filepath.Base(dir) + " " + at.Format("2006-01-02 15.04.05")
	for i := 1; ; i++ {
		target := filepath.Join(bin, name)
		if i > 1 {
			target = fmt.Sprintf("%s %d", target, i)
		}
		if _, err := os.Lstat(target); err == nil {
			continue
		}
		err := os.Rename(dir, target)
		if errors.Is(err, syscall.EXDEV) {
			return fmt.Errorf("%w: %s is on another disk; delete it in Finder", ErrInvalidOperation, filepath.Base(dir))
		}
		return wrap(err)
	}
}
