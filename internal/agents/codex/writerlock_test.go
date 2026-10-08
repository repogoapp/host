package codex

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/repogo/host/internal/agent"
)

// holdWriterLock lays out Codex's lock files under home and, when held,
// takes threadID's lock the way another Codex process would.
func holdWriterLock(t *testing.T, home, threadID string, held bool) {
	t.Helper()
	dir := filepath.Join(home, "thread-writer-locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".coordination.lock", threadID + ".lock"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if !held {
		return
	}
	lock, err := os.Open(filepath.Join(dir, threadID+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
}

func TestThreadOpenElsewhereReadsCodexWriterLock(t *testing.T) {
	home := t.TempDir()
	if threadOpenElsewhere(home, "thread-a") {
		t.Fatal("no lock directory counted as open")
	}
	holdWriterLock(t, home, "thread-a", false)
	if threadOpenElsewhere(home, "thread-a") {
		t.Fatal("unheld lock counted as open")
	}
	holdWriterLock(t, home, "thread-b", true)
	if !threadOpenElsewhere(home, "thread-b") {
		t.Fatal("held lock not seen")
	}
	if threadOpenElsewhere(home, "thread-a") {
		t.Fatal("probe left thread-a locked")
	}
}

func TestSendToThreadOpenElsewhereFailsBeforeStartingCodex(t *testing.T) {
	r := testRunner(t)
	r.executable = func() string { return filepath.Join(t.TempDir(), "no-codex") }
	holdWriterLock(t, r.home, "thread-b", true)
	_, _, _, err := turnOf(t, r, agent.TurnRequest{ChatID: "codex:thread-b", SessionID: "thread-b", Cwd: t.TempDir(), Prompt: "hello"}, nil)
	if !errors.Is(err, errOpenElsewhere) {
		t.Fatalf("got %v", err)
	}
}
