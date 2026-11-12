package codex

import (
	"errors"
	"os"

	win "golang.org/x/sys/windows"
)

// The whole file, as Codex locks it on Windows (LockFileEx over every byte).
const allBytes = ^uint32(0)

// lockFile takes f's exclusive lock, waiting for it.
func lockFile(f *os.File) error {
	return win.LockFileEx(win.Handle(f.Fd()), win.LOCKFILE_EXCLUSIVE_LOCK, 0, allBytes, allBytes, &win.Overlapped{})
}

// tryLockFile takes f's exclusive lock if it is free; held is true when
// another process has it.
func tryLockFile(f *os.File) (held bool, err error) {
	err = win.LockFileEx(win.Handle(f.Fd()),
		win.LOCKFILE_EXCLUSIVE_LOCK|win.LOCKFILE_FAIL_IMMEDIATELY, 0, allBytes, allBytes, &win.Overlapped{})
	if errors.Is(err, win.ERROR_LOCK_VIOLATION) || errors.Is(err, win.ERROR_IO_PENDING) {
		return true, err
	}
	return false, err
}

func unlockFile(f *os.File) {
	_ = win.UnlockFileEx(win.Handle(f.Fd()), 0, allBytes, allBytes, &win.Overlapped{})
}
