package store

import (
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
)

func handles(t *testing.T, db *Store) map[ChatID]*VoiceHandle {
	t.Helper()
	rows, err := db.queryChats(chatSelect)
	if err != nil {
		t.Fatalf("chat rows: %v", err)
	}
	out := map[ChatID]*VoiceHandle{}
	for _, c := range rows {
		out[c.ID] = c.VoiceHandle
	}
	return out
}

func TestEveryNewChatGetsAUniqueHandleThatSurvivesAResync(t *testing.T) {
	db := newStore(t)
	for i, id := range []string{"a", "b", "c"} {
		write(t, db, id, int64(1000+i), 2)
	}
	first := handles(t, db)
	seen := map[string]bool{}
	for id, h := range first {
		if h == nil || h.Display != h.Prefix+" "+h.Name || h.Key != voiceKey(h.Prefix, h.Name) {
			t.Fatalf("%s: bad handle %+v", id, h)
		}
		if seen[h.Key] {
			t.Fatalf("%s: duplicate handle %s", id, h.Key)
		}
		seen[h.Key] = true
	}

	write(t, db, "a", 5000, 3)
	if got := handles(t, db)[chatID("claude", "a")]; *got != *first[chatID("claude", "a")] {
		t.Fatalf("resync renamed the chat: %+v -> %+v", first[chatID("claude", "a")], got)
	}
	info, err := db.Info(chatID("claude", "b"))
	if err != nil || info.VoiceHandle == nil || *info.VoiceHandle != *first[info.ID] {
		t.Fatalf("info handle = %+v, %v", info.VoiceHandle, err)
	}
}

func TestAStatusRowIsNamedBeforeItsTranscript(t *testing.T) {
	db := newStore(t)
	id := chatID(string(agent.KindClaude), "live")
	if err := db.SetStatus(id, "/tmp/live", agent.ChatWorking, time.UnixMilli(10), TurnObservation{}); err != nil {
		t.Fatalf("set status: %v", err)
	}
	if handles(t, db)[id] == nil {
		t.Fatal("a chat started from a hook has no handle")
	}
}

func TestAFullPoolRecyclesTheStalestHandle(t *testing.T) {
	prefixes, names, size := voicePrefixes, voiceNames, voicePoolSize
	voicePrefixes, voiceNames, voicePoolSize = []string{"Blue", "Gold"}, []string{"Otter"}, 2
	t.Cleanup(func() { voicePrefixes, voiceNames, voicePoolSize = prefixes, names, size })

	db := newStore(t)
	write(t, db, "old", 100, 1)
	write(t, db, "mid", 200, 1)
	oldHandle := handles(t, db)[chatID("claude", "old")]
	before, _ := db.queryChats(chatSelect)

	write(t, db, "new", 300, 1)
	got := handles(t, db)
	if got[chatID("claude", "old")] != nil {
		t.Fatal("the stalest chat kept its handle past the cap")
	}
	if h := got[chatID("claude", "new")]; h == nil || *h != *oldHandle {
		t.Fatalf("the new chat got %+v, want the recycled %+v", h, oldHandle)
	}
	// The chat that lost its name must reach a mirroring device.
	after, _ := db.queryChats(chatSelect)
	revOf := func(rows []Chat, id ChatID) int64 {
		for _, c := range rows {
			if c.ID == id {
				return c.Rev
			}
		}
		return 0
	}
	if revOf(after, chatID("claude", "old")) <= revOf(before, chatID("claude", "old")) {
		t.Fatal("releasing a handle did not bump the row's revision")
	}

	// A chat older than every holder waits rather than taking a live name.
	write(t, db, "older", 50, 1)
	if handles(t, db)[chatID("claude", "older")] != nil {
		t.Fatal("an older chat took a handle from a newer one")
	}
}
