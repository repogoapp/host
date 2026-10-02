package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// An update with --now stops every running turn and holds the queued ones for
// the restarted host (TestStopAllHoldsQueuedTurns), and a turn sent while the
// guard is up is refused rather than started under the old binary.
func TestStopAllEndsRunningTurns(t *testing.T) {
	m := NewManager(discard(), quiet(), blockingAdapter{})
	for _, chat := range []string{"claude:a", "claude:a", "claude:b"} {
		if _, err := m.Send(TurnRequest{ChatID: chat, Cwd: t.TempDir(), Agent: KindClaude, Prompt: "go"}); err != nil {
			t.Fatal(err)
		}
	}
	if got := m.ActiveChats(); got != 2 {
		t.Fatalf("active chats %d, want 2", got)
	}
	if err := m.GuardUpdate(true, false); !errors.Is(err, ErrUpdating) {
		t.Fatalf("guard over running chats: %v, want ErrUpdating", err)
	}
	if err := m.GuardUpdate(true, true); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.StopAll(ctx); err != nil {
		t.Fatal(err)
	}
	if got := m.ActiveChats(); got != 0 {
		t.Fatalf("active chats %d after StopAll, want 0", got)
	}
	for _, turn := range m.List() {
		if turn.State == StateRunning {
			t.Fatalf("turn %s still running", turn.TurnID)
		}
	}
	if _, err := m.Send(TurnRequest{ChatID: "claude:c", Cwd: t.TempDir(), Agent: KindClaude, Prompt: "late"}); !errors.Is(err, ErrUpdating) {
		t.Fatalf("send during update: %v, want ErrUpdating", err)
	}
}
