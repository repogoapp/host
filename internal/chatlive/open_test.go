package chatlive

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/store"
	"github.com/repogoapp/host/internal/testwait"
)

// seeded is a temporary home with live-1 synced into a fresh cache, and a
// Manager serving it to pub.
func seeded(t *testing.T, lines ...string) (home string, db *store.Store, m *Manager, pub *capture) {
	t.Helper()
	home, sessions := liveHome(t, lines...)
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	metas, _ := sessions.List()
	if len(metas) == 0 {
		t.Fatal("session discovery found nothing in the temporary home")
	}
	events, err := sessions.Events(metas[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Sync(metas[0], events); err != nil {
		t.Fatal(err)
	}
	pub = &capture{}
	return home, db, newManager(t, sessions, db, pub, &fakeTurns{}), pub
}

const (
	phone  = device.ID("00112233445566778899aabbccddeeff")
	tablet = device.ID("ffeeddccbbaa99887766554433221100")
)

// Resuming with a cursor answers with only the rows after it, and no reset.
func TestOpenWithACursorAnswersTheRowsAfterIt(t *testing.T) {
	home, db, m, _ := seeded(t, userLine("first"), userLine("second"))
	first, err := m.Open(phone, "claude:live-1", nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	m.Unsubscribe(phone)
	writeClaudeSession(t, home, "live-1", []string{userLine("third")})
	syncNow(t, m)

	have := &store.ChatCursor{CacheEpoch: first.Page.CacheEpoch, Generation: first.Page.Generation, NextIndex: first.Page.NextIndex}
	live, err := m.Open(phone, "claude:live-1", have, 500)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Unsubscribe(phone)
	if live.Reset || len(live.Page.Events) != 1 || live.Page.Events[0].Index != have.NextIndex {
		t.Fatalf("resume = reset %v, %d rows from %d; want the one row from %d",
			live.Reset, len(live.Page.Events), live.Page.FirstIndex, have.NextIndex)
	}
	if live.Page.CacheEpoch != db.Epoch() {
		t.Errorf("resume page epoch %q, want %q", live.Page.CacheEpoch, db.Epoch())
	}
}

// A cursor from another cache, another generation or past the end no longer
// holds: the reply is the newest rows, with reset.
func TestOpenWithACursorThatNoLongerHoldsResets(t *testing.T) {
	_, db, m, _ := seeded(t, userLine("first"), userLine("second"))
	head, err := db.Tail("claude:live-1", 10)
	if err != nil {
		t.Fatal(err)
	}
	for name, have := range map[string]store.ChatCursor{
		"another cache":      {CacheEpoch: "elsewhere", Generation: head.Generation, NextIndex: 1},
		"another generation": {CacheEpoch: db.Epoch(), Generation: head.Generation + 1, NextIndex: 1},
		"past the end":       {CacheEpoch: db.Epoch(), Generation: head.Generation, NextIndex: head.EventCount + 5},
	} {
		live, err := m.Open(phone, "claude:live-1", &have, 10)
		if err != nil {
			t.Fatal(err)
		}
		if !live.Reset || live.Page.NextIndex != head.EventCount || len(live.Page.Events) != len(head.Events) {
			t.Errorf("%s: reset %v, page %d rows to %d; want reset and the tail", name, live.Reset,
				len(live.Page.Events), live.Page.NextIndex)
		}
	}
	m.Unsubscribe(phone)
}

// Two devices on one chat share one watch, and each gets the new row once.
func TestTwoDevicesShareOneWatch(t *testing.T) {
	home, _, m, pub := seeded(t, userLine("first"))
	a, err := m.Open(phone, "claude:live-1", nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Open(tablet, "claude:live-1", nil, 100); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	watches := len(m.watches)
	m.mu.Unlock()
	if watches != 1 {
		t.Fatalf("%d watches for one chat, want 1", watches)
	}

	writeClaudeSession(t, home, "live-1", []string{userLine("second")})
	for _, d := range []device.ID{phone, tablet} {
		testwait.For(t, "a push to "+string(d), func() bool { return len(rowsTo(pub, d)) > 0 })
		rows := rowsTo(pub, d)
		if rows[0] != a.Page.NextIndex || len(rows) != 1 {
			t.Errorf("device %s got rows %v, want only %d", d, rows, a.Page.NextIndex)
		}
	}

	// The last device leaving stops the watch.
	m.Unsubscribe(phone)
	m.Unsubscribe(tablet)
	m.mu.Lock()
	watches = len(m.watches)
	m.mu.Unlock()
	if watches != 0 {
		t.Errorf("%d watches after every device left, want 0", watches)
	}
}

// A device on a generation the transcript no longer has hears so, even when
// the rewrite left fewer rows than it holds and nothing is new.
func TestARewriteToFewerRowsIsAnnounced(t *testing.T) {
	home, _, m, pub := seeded(t, userLine("first"), userLine("second"), userLine("third"))
	live, err := m.Open(phone, "claude:live-1", nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Unsubscribe(phone)

	// The transcript is rewound to its first line.
	path := filepath.Join(home, ".claude", "projects", "-srv-demo", "live-1.jsonl")
	if err := os.WriteFile(path, []byte(userLine("first")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	os.Chtimes(path, future, future)
	m.Nudge("claude:live-1")

	testwait.For(t, "a notice of the new generation", func() bool {
		for _, p := range pub.receivedBy(phone) {
			if p.Generation != live.Page.Generation {
				return true
			}
		}
		return false
	})
	for _, p := range pub.receivedBy(phone) {
		if p.Generation != live.Page.Generation && (len(p.Events) != 0 || p.NextIndex != p.EventCount) {
			t.Errorf("notice = %d rows, next %d of %d; want no rows at the end", len(p.Events), p.NextIndex, p.EventCount)
		}
	}
}

// A device re-opening the chat it watches keeps the chat's watch: a new one
// would replay the whole transcript to start.
func TestReopeningTheSameChatKeepsItsWatch(t *testing.T) {
	_, _, m, _ := seeded(t, userLine("first"))
	first, err := m.Open(phone, "claude:live-1", nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Unsubscribe(phone)
	m.mu.Lock()
	before := m.watches["claude:live-1"]
	m.mu.Unlock()

	have := &store.ChatCursor{CacheEpoch: first.Page.CacheEpoch, Generation: first.Page.Generation, NextIndex: first.Page.NextIndex}
	if _, err := m.Open(phone, "claude:live-1", have, 500); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	after := m.watches["claude:live-1"]
	m.mu.Unlock()
	if after == nil || after != before {
		t.Fatal("a resume of the same chat replaced its watch")
	}
}

// rowsTo is the indexes of every row pushed to d.
func rowsTo(pub *capture, d device.ID) []int {
	var out []int
	for _, p := range pub.receivedBy(d) {
		for _, e := range p.Events {
			out = append(out, e.Index)
		}
	}
	return out
}

// syncNow brings the cache up to date with live-1's file, as a sweep would.
func syncNow(t *testing.T, m *Manager) {
	t.Helper()
	meta, _, err := m.sessions.Find("live-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.resync(meta); err != nil {
		t.Fatal(err)
	}
}
