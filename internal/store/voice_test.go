package store

import (
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
)

func numbers(t *testing.T, db *Store) map[ChatID]int64 {
	t.Helper()
	rows, err := db.queryChats(chatSelect)
	if err != nil {
		t.Fatalf("chat rows: %v", err)
	}
	out := map[ChatID]int64{}
	for _, c := range rows {
		out[c.ID] = c.VoiceNumber
	}
	return out
}

func TestEveryNewChatGetsTheNextNumberAndKeepsItAcrossAResync(t *testing.T) {
	db := newStore(t)
	for i, id := range []string{"a", "b", "c"} {
		write(t, db, id, int64(1000+i), 2)
	}
	first := numbers(t, db)
	for i, id := range []string{"a", "b", "c"} {
		if got := first[chatID("claude", id)]; got != int64(i+1) {
			t.Fatalf("%s: number %d, want %d", id, got, i+1)
		}
	}

	write(t, db, "a", 5000, 3)
	if got := numbers(t, db)[chatID("claude", "a")]; got != first[chatID("claude", "a")] {
		t.Fatalf("resync renumbered the chat: %d -> %d", first[chatID("claude", "a")], got)
	}
	info, err := db.Info(chatID("claude", "b"))
	if err != nil || info.VoiceNumber != first[info.ID] {
		t.Fatalf("info number = %d, %v", info.VoiceNumber, err)
	}
}

func TestAStatusRowIsNumberedBeforeItsTranscript(t *testing.T) {
	db := newStore(t)
	id := chatID(string(agent.KindClaude), "live")
	if err := db.SetStatus(id, "/tmp/live", agent.ChatWorking, time.UnixMilli(10), TurnObservation{}); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if numbers(t, db)[id] == 0 {
		t.Fatal("a chat started from a hook has no number")
	}
}

// A number the user has heard keeps meaning one chat, so a deleted chat's
// number is never handed to a new one.
func TestADeletedChatsNumberIsNotReused(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 100, 1)
	write(t, db, "b", 200, 1)
	if err := db.Delete(chatID("claude", "b")); err != nil {
		t.Fatal(err)
	}
	write(t, db, "c", 300, 1)
	if got := numbers(t, db)[chatID("claude", "c")]; got != 3 {
		t.Fatalf("new chat's number = %d, want 3", got)
	}
}

func TestOpenNumbersChatsWithoutANumber(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, db, "a", 100, 1)
	if _, err := db.db.Exec(`DELETE FROM state.voice_numbers`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if numbers(t, db)[chatID("claude", "a")] == 0 {
		t.Fatal("open left a chat without a number")
	}
}
