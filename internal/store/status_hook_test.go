package store

import (
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
)

// Each recorded status reaches the agent clock with its chat's folder and turn,
// and a turn settled at start reports its last sign of life, not the settling.
func TestStatusesReachTheAgentClock(t *testing.T) {
	db := newStore(t)
	var heard []StatusChange
	db.OnStatus(func(c StatusChange) { heard = append(heard, c) })

	start := time.UnixMilli(1_793_534_400_000)
	turn := TurnObservation{Key: ManagerTurnPrefix + "1", StartedAt: &start}
	if err := db.SetStatus("claude:c1", "/w/app", agent.ChatWorking, start, turn); err != nil {
		t.Fatal(err)
	}
	if len(heard) != 1 || heard[0].Cwd != "/w/app" || heard[0].TurnKey != turn.Key ||
		heard[0].Status != agent.ChatWorking || !heard[0].At.Equal(start) {
		t.Fatalf("heard %+v", heard)
	}

	settled := start.Add(time.Hour)
	if n, err := db.InterruptOrphans(settled); err != nil || n != 1 {
		t.Fatalf("interrupted %d, %v", n, err)
	}
	last := heard[len(heard)-1]
	if last.Status != agent.ChatInterrupted || !last.At.Before(settled) || last.Cwd != "/w/app" {
		t.Fatalf("interrupt reported %+v, want its last sign of life before %v", last, settled)
	}
	if seen, ok := db.LastSeen("claude:c1"); !ok || seen.Before(start) {
		t.Fatalf("last seen %v, %v", seen, ok)
	}
}
