//go:build unix

package files

import (
	"os"

	"golang.org/x/sys/unix"
)

// open reads the leaf without following it: os.Root follows a relative
// symlink inside the root, and openat with O_NOFOLLOW refuses one.
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
