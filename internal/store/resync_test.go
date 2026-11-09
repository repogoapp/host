package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
)

// Each change a transcript can go through, synced over the one before it,
// leaves the same rows and generation behaviour as syncing it whole, and
// writes only from the first row that changed.
func TestResyncWritesFromTheFirstChangedRow(t *testing.T) {
	events := func(n int) []agent.Event {
		out := make([]agent.Event, n)
		for i := range out {
			out[i] = agent.Event{Kind: agent.EventText, Text: fmt.Sprintf("row %d", i), At: int64(1000 + i)}
		}
		out[0].Kind = agent.EventUserMessage
		return out
	}
	edited := events(40)
	edited[25].Tool = &agent.ToolCall{CallID: "c", Name: "Bash", Output: "late result"}

	steps := []struct {
		name   string
		events []agent.Event
		keep   int  // rows the step must leave in place
		bump   bool // whether a device must reload
	}{
		{"first", events(10), 0, false},
		{"append", events(40), 10, false},
		{"no change", events(40), 40, false},
		{"edit row 25", edited, 25, true},
		{"rewind to 30", edited[:30], 30, true},
		{"grow again", events(50), 25, true},
	}

	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	gen := 0
	for i, step := range steps {
		meta := session.Meta{ID: "s", Agent: "claude", Path: "/s.jsonl",
			UpdatedAt: time.UnixMilli(int64(i + 1)), SizeBytes: int64(i + 1)}
		// Rows a step keeps are untouched: mark them and see the marks survive.
		if _, err := db.db.Exec(`UPDATE events SET at = -1 WHERE idx < ?`, step.keep); err != nil {
			t.Fatal(err)
		}
		if err := db.SyncBatch([]Entry{{Meta: meta, Events: step.events, DeferSearch: i%2 == 0}}); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		var marked, count int
		db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE at = -1`).Scan(&marked)
		db.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&count)
		if marked != step.keep || count != len(step.events) {
			t.Errorf("%s: kept %d of %d rows, want %d of %d", step.name, marked, count, step.keep, len(step.events))
		}
		if _, err := db.db.Exec(`UPDATE events SET at = 1000 + idx WHERE at = -1`); err != nil {
			t.Fatal(err)
		}
		var g int
		db.db.QueryRow(`SELECT generation FROM sessions`).Scan(&g)
		if bumped := g > gen && i > 0; bumped != step.bump {
			t.Errorf("%s: generation %d after %d, want bump %v", step.name, g, gen, step.bump)
		}
		gen = g

		// The rows read back exactly as a sync of the whole transcript writes them.
		rows := eventRows(step.events)
		got, err := db.Tail("claude:s", 500)
		if err != nil {
			t.Fatal(err)
		}
		want := MessagesOf(step.events)
		if len(got.Events) != len(want) {
			t.Fatalf("%s: %d rows read back, want %d", step.name, len(got.Events), len(want))
		}
		for j := range want {
			if got.Events[j].Text != want[j].Text || got.Events[j].Tool != want[j].Tool {
				t.Fatalf("%s: row %d = %+v, want %+v", step.name, j, got.Events[j], want[j])
			}
		}
		var hash int64
		db.db.QueryRow(`SELECT content_hash FROM sessions`).Scan(&hash)
		if hash != chainOf(rows, len(rows)) {
			t.Errorf("%s: content_hash is not the last row's chain", step.name)
		}
	}
}

// A live flush leaves search to the sweep; IndexStale then indexes what a
// sweep would have.
func TestDeferredSearchIsIndexedByTheSweep(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	meta := session.Meta{ID: "s", Agent: "claude", Path: "/s.jsonl", UpdatedAt: time.UnixMilli(1), SizeBytes: 1}
	events := []agent.Event{
		{Kind: agent.EventUserMessage, Text: "find the walrus"},
		{Kind: agent.EventToolCall, Tool: &agent.ToolCall{CallID: "c", Name: "Bash"}},
		{Kind: agent.EventText, Text: "the walrus is in the attic"},
	}
	if err := db.SyncBatch([]Entry{{Meta: meta, Events: events, DeferSearch: true}}); err != nil {
		t.Fatal(err)
	}
	var stale int
	db.db.QueryRow(`SELECT search_stale FROM sessions`).Scan(&stale)
	if stale != 1 {
		t.Fatal("a deferred sync left the chat unmarked; no sweep would index it")
	}
	if n, err := db.IndexStale(); err != nil || n != 1 {
		t.Fatalf("IndexStale = %d, %v", n, err)
	}
	var body string
	db.db.QueryRow(`SELECT body FROM chat_search`).Scan(&body)
	if want := indexText(searchBody(events)); body != want {
		t.Errorf("indexed %q, want %q", body, want)
	}
	if n, _ := db.IndexStale(); n != 0 {
		t.Errorf("a second sweep reindexed %d chats", n)
	}
}
