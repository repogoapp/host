//go:build unix

package codex

import (
	"errors"
	"os"
	"syscall"
)

// lockFile takes f's exclusive lock, waiting for it, as Codex's flock does.
func lockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }

// tryLockFile takes f's exclusive lock if it is free; held is true when
// another process has it.
func tryLockFile(f *os.File) (held bool, err error) {
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true, err
	}
	return false, err
}

func unlockFile(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
