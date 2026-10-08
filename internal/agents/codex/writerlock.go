package codex

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// threadOpenElsewhere reports whether another Codex process (a terminal, the
// desktop app) holds threadID's writer lock. It takes Codex's coordination
// lock first, as Codex's own writers do, so a writer racing it waits instead of failing.
func threadOpenElsewhere(home, threadID string) bool {
	dir := filepath.Join(home, "thread-writer-locks")
	coordination, err := os.OpenFile(filepath.Join(dir, ".coordination.lock"), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer coordination.Close()
	if syscall.Flock(int(coordination.Fd()), syscall.LOCK_EX) != nil {
		return false
	}
	defer syscall.Flock(int(coordination.Fd()), syscall.LOCK_UN)

	thread, err := os.OpenFile(filepath.Join(dir, threadID+".lock"), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer thread.Close()
	err = syscall.Flock(int(thread.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true
	}
	if err == nil {
		_ = syscall.Flock(int(thread.Fd()), syscall.LOCK_UN)
	}
	return false
}
