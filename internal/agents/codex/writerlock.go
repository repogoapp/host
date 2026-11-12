package codex

import (
	"os"
	"path/filepath"
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
	if lockFile(coordination) != nil {
		return false
	}
	defer unlockFile(coordination)

	thread, err := os.OpenFile(filepath.Join(dir, threadID+".lock"), os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer thread.Close()
	held, err := tryLockFile(thread)
	if held {
		return true
	}
	if err == nil {
		unlockFile(thread)
	}
	return false
}
