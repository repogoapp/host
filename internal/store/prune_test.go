package store

import (
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
)

func TestPruneDropsSessionsMissingFromListing(t *testing.T) {
	db := newStore(t)
	write(t, db, "keep", 1, 2)
	write(t, db, "gone", 2, 3)
	// A live chat whose transcript has not been written yet.
	if err := db.SetStatus(chatID("claude", "live"), "/tmp/live", agent.ChatWorking, time.Now(), TurnObservation{}); err != nil {
		t.Fatalf("set status: %v", err)
	}

	if err := db.SetResolved(chatID("claude", "gone"), true, time.Now()); err != nil {
		t.Fatal(err)
	}

	pruned, err := db.Prune(map[string]bool{"claude\x00keep": true})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 1 {
		t.Fatalf("pruned %d sessions, want 1", pruned)
	}

	var sessions, events int64
	if sessions, events, err = db.Counts(); err != nil {
		t.Fatal(err)
	}
	if sessions != 2 {
		t.Errorf("sessions = %d, want 2 (kept + live placeholder)", sessions)
	}
	if events != 2 {
		t.Errorf("events = %d, want 2 (only the kept session's)", events)
	}
	var marks, handles int
	if err := db.db.QueryRow(`SELECT (SELECT COUNT(*) FROM chat_marks), (SELECT COUNT(*) FROM voice_handles)`).Scan(&marks, &handles); err != nil {
		t.Fatal(err)
	}
	if marks != 0 || handles != 2 {
		t.Errorf("marks = %d, handles = %d; want the pruned chat's gone", marks, handles)
	}
}
