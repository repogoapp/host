package files

import (
	"os"
	"syscall"
)

// open reads the leaf without following it. Windows has no O_NOFOLLOW, so a
// symlink or junction at the leaf is refused before the open; os.Root still
// keeps the open inside the root.
func (p rootedPath) open() (*os.File, error) {
	info, err := p.root.Lstat(p.name)
	if err != nil {
		return nil, err
	}
	if info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return nil, &os.PathError{Op: "open", Path: p.full, Err: syscall.ELOOP}
	}
	return p.root.Open(p.name)
}
