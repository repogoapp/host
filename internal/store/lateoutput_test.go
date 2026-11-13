package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
)

// sameButOutput is the seam a host sets, in miniature: two rows read the same
// to a phone when only a tool's output differs and neither failed.
func sameButOutput(_ string, old, new Message) bool {
	strip := func(m Message) (Message, bool) {
		var call agent.ToolCall
		if m.Tool != "" {
			json.Unmarshal([]byte(m.Tool), &call)
			failed := call.IsError
			call.Output, call.IsError = "", false
			b, _ := json.Marshal(call)
			m.Tool = string(b)
			return m, failed
		}
		return m, false
	}
	a, aFailed := strip(old)
	b, bFailed := strip(new)
	return a == b && aFailed == bFailed
}

func lateOutputEvents(output string, failed bool, more int) []agent.Event {
	events := []agent.Event{
		{Kind: agent.EventUserMessage, Text: "audit it", At: 1},
		{Kind: agent.EventToolCall, Tool: &agent.ToolCall{CallID: "c", Name: "Agent"}, At: 2},
		{Kind: agent.EventToolResult, Tool: &agent.ToolCall{CallID: "c", Output: output, IsError: failed}, At: 3},
		{Kind: agent.EventText, Text: "It is running.", At: 4},
	}
	for i := 0; i < more; i++ {
		events = append(events, agent.Event{Kind: agent.EventText, Text: "later", At: int64(5 + i)})
	}
	return events
}

func lateOutputMeta(step int) session.Meta {
	return session.Meta{ID: "s", Agent: "claude", Path: "/s.jsonl",
		UpdatedAt: time.UnixMilli(int64(step)), SizeBytes: int64(step)}
}

func generationOf(t *testing.T, db *Store) int64 {
	t.Helper()
	var g int64
	if err := db.db.QueryRow(`SELECT generation FROM sessions`).Scan(&g); err != nil {
		t.Fatal(err)
	}
	return g
}

func outputOf(t *testing.T, db *Store) string {
	t.Helper()
	rows, err := db.ToolCalls("claude:s", []string{"c"})
	if err != nil {
		t.Fatal(err)
	}
	var out string
	for _, m := range rows {
		var call agent.ToolCall
		json.Unmarshal([]byte(m.Tool), &call)
		if call.Output != "" {
			out = call.Output
		}
	}
	return out
}

// A tool's output arriving late rewrites its stored row, and the rows after
// it, without telling devices their history is gone: the rows they hold read
// the same, so a resume from an older cursor is answered, not reset.
func TestALateOutputKeepsTheGeneration(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SameRow(sameButOutput)

	if err := db.Sync(lateOutputMeta(1), lateOutputEvents("Async agent launched successfully.", false, 0)); err != nil {
		t.Fatal(err)
	}
	opened, _, _, err := db.Open("claude:s", nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	have := &ChatCursor{CacheEpoch: opened.CacheEpoch, Generation: opened.Generation, NextIndex: opened.NextIndex}

	if err := db.Sync(lateOutputMeta(2), lateOutputEvents("Found two bugs.", false, 1)); err != nil {
		t.Fatal(err)
	}
	if got := generationOf(t, db); got != opened.Generation {
		t.Errorf("generation %d after a late output, want %d still", got, opened.Generation)
	}
	if got := outputOf(t, db); got != "Found two bugs." {
		t.Errorf("stored output %q, want the late one", got)
	}
	page, _, reset, err := db.Open("claude:s", have, 100)
	if err != nil {
		t.Fatal(err)
	}
	if reset || len(page.Events) != 1 || page.Events[0].Index != 4 {
		t.Errorf("resume: reset %v with %d rows, want only the appended row", reset, len(page.Events))
	}
}

// What a device holds did change: it reloads, as before.
func TestAChangedRowStillBumpsTheGeneration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		seam   bool
		events []agent.Event
	}{
		{"the late result failed", true, lateOutputEvents("It crashed.", true, 0)},
		{"fewer rows than before", true, lateOutputEvents("Found two bugs.", false, 0)[:3]},
		{"no seam set", false, lateOutputEvents("Found two bugs.", false, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if tc.seam {
				db.SameRow(sameButOutput)
			}
			if err := db.Sync(lateOutputMeta(1), lateOutputEvents("Async agent launched successfully.", false, 0)); err != nil {
				t.Fatal(err)
			}
			before := generationOf(t, db)
			if err := db.Sync(lateOutputMeta(2), tc.events); err != nil {
				t.Fatal(err)
			}
			if got := generationOf(t, db); got == before {
				t.Errorf("generation stayed %d, want a bump", got)
			}
		})
	}
}

// The sweep and a chat's flush read the file apart. An older read holds the
// same number of rows as the fold that followed it, so the count alone would
// let it write the launch answer back over the late result.
func TestAnOlderReadDoesNotUndoALateOutput(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SameRow(sameButOutput)

	launched := lateOutputEvents("Async agent launched successfully.", false, 0)
	if err := db.Sync(lateOutputMeta(1), launched); err != nil {
		t.Fatal(err)
	}
	if err := db.Sync(lateOutputMeta(3), lateOutputEvents("Found two bugs.", false, 0)); err != nil {
		t.Fatal(err)
	}
	before := generationOf(t, db)
	if err := db.Sync(lateOutputMeta(2), launched); err != nil {
		t.Fatal(err)
	}
	if got := outputOf(t, db); got != "Found two bugs." {
		t.Errorf("stored output %q after an older read, want the late one kept", got)
	}
	if got := generationOf(t, db); got != before {
		t.Errorf("generation %d after an older read, want %d still", got, before)
	}
}
