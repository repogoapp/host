package store

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
)

func parentMeta(id string) session.Meta { return session.Meta{Agent: agent.KindClaude, ID: id} }

// childFile writes a transcript file so a child has a fingerprint to compare.
func childFile(t *testing.T, size int) session.ChildFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.jsonl")
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	return session.ChildFile{Path: path, Size: int64(size), ModTime: time.UnixMilli(5000)}
}

// Children are written beside their chat and read back by call; the chat
// list, its counts and its events never see them.
func TestSyncSubagentsKeepsChildrenBesideTheirChat(t *testing.T) {
	db := newStore(t)
	write(t, db, "p", 1, 2)
	task := session.Child{Key: "a9", CallID: "toolu_task", Kind: "Explore", Files: []session.ChildFile{childFile(t, 10)}}
	wf := session.Child{Key: "w1", CallID: "toolu_wf", RunID: "wf_1", Label: "map", Phase: "Map", Files: []session.ChildFile{childFile(t, 20)}}

	changed, err := db.SyncSubagents(parentMeta("p"), []session.Child{task, wf}, map[string][]agent.Event{
		"a9": {{Kind: agent.EventText, Text: "found it", At: 4000}},
		"w1": {{Kind: agent.EventText, Text: "mapping", At: 4500}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []string{"toolu_task", "toolu_wf"}) {
		t.Errorf("changed = %v, want both calls", changed)
	}

	sub, rows, err := db.SubagentMessages(chatID("claude", "p"), "toolu_task", "")
	if err != nil || sub.Kind != "Explore" || sub.StartedAt != 4000 || len(rows) != 1 || rows[0].Text != "found it" {
		t.Fatalf("toolu_task = %+v, %+v, %v", sub, rows, err)
	}
	fps, err := db.ChildFingerprints(parentMeta("p"))
	if err != nil || fps["a9"] != task.Fingerprint() || fps["w1"] != wf.Fingerprint() {
		t.Fatalf("fingerprints = %v, %v; want each child's as read", fps, err)
	}

	sessions, events, err := db.Counts()
	if err != nil || sessions != 1 || events != 2 {
		t.Errorf("counts = %d sessions, %d events, %v; want only the chat's", sessions, events, err)
	}
	page, err := db.Chats(ChatQuery{Limit: 10})
	if err != nil || len(page.Chats) != 1 {
		t.Errorf("chats = %+v, %v; want only the parent", page.Chats, err)
	}
}

// A pass that finds nothing new writes nothing; a finished agent, a dropped
// one and a fresh transcript each name their call.
func TestSyncSubagentsReportsOnlyWhatMoved(t *testing.T) {
	db := newStore(t)
	write(t, db, "p", 1, 1)
	w1 := session.Child{Key: "w1", CallID: "toolu_wf", Label: "map", Files: []session.ChildFile{childFile(t, 10)}}
	w2 := session.Child{Key: "w2", CallID: "toolu_wf", Label: "build", Order: 1}
	a9 := session.Child{Key: "a9", CallID: "toolu_task"}
	if _, err := db.SyncSubagents(parentMeta("p"), []session.Child{w1, w2, a9}, map[string][]agent.Event{"w1": {}}); err != nil {
		t.Fatal(err)
	}

	if changed, err := db.SyncSubagents(parentMeta("p"), []session.Child{w1, w2, a9}, nil); err != nil || len(changed) != 0 {
		t.Fatalf("unchanged pass = %v, %v; want nothing", changed, err)
	}

	w1.Done, w1.Result = true, "mapped"
	if changed, err := db.SyncSubagents(parentMeta("p"), []session.Child{w1, w2, a9}, nil); err != nil || !slices.Equal(changed, []string{"toolu_wf"}) {
		t.Fatalf("finished agent = %v, %v; want toolu_wf", changed, err)
	}

	if changed, err := db.SyncSubagents(parentMeta("p"), []session.Child{w1, w2}, nil); err != nil || !slices.Equal(changed, []string{"toolu_task"}) {
		t.Fatalf("dropped agent = %v, %v; want toolu_task", changed, err)
	}

	calls, err := db.SubagentCalls(chatID("claude", "p"))
	if err != nil || len(calls) != 1 || calls[0].CallID != "toolu_wf" || calls[0].Agents != 2 || calls[0].Done != 1 {
		t.Fatalf("calls = %+v, %v; want toolu_wf with 2 agents, 1 done", calls, err)
	}
}

// A child whose transcript was not read keeps no fingerprint, so the next
// pass reads it.
func TestSyncSubagentsLeavesAnUnreadChildToRead(t *testing.T) {
	db := newStore(t)
	write(t, db, "p", 1, 1)
	child := session.Child{Key: "a9", CallID: "toolu_task", Files: []session.ChildFile{childFile(t, 10)}}
	if _, err := db.SyncSubagents(parentMeta("p"), []session.Child{child}, nil); err != nil {
		t.Fatal(err)
	}
	if fps, _ := db.ChildFingerprints(parentMeta("p")); fps["a9"] == child.Fingerprint() {
		t.Fatal("an unread child was marked read")
	}
}

// Children wait for their chat's first sync, and go with it.
func TestSubagentsFollowTheirChat(t *testing.T) {
	db := newStore(t)
	child := session.Child{Key: "a9", CallID: "toolu_task"}
	if changed, err := db.SyncSubagents(parentMeta("p"), []session.Child{child}, map[string][]agent.Event{"a9": {}}); err != nil || changed != nil {
		t.Fatalf("before the chat = %v, %v; want nothing written", changed, err)
	}

	write(t, db, "p", 1, 1)
	write(t, db, "q", 1, 1)
	for _, id := range []string{"p", "q"} {
		if _, err := db.SyncSubagents(parentMeta(id), []session.Child{child}, map[string][]agent.Event{"a9": {{Kind: agent.EventText, Text: "x"}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Delete(chatID("claude", "p")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Prune(map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	var children, events int
	if err := db.db.QueryRow(`SELECT (SELECT COUNT(*) FROM subagents), (SELECT COUNT(*) FROM subagent_events)`).Scan(&children, &events); err != nil {
		t.Fatal(err)
	}
	if children != 0 || events != 0 {
		t.Errorf("left %d children, %d events; want none after delete and prune", children, events)
	}
}

// A call's revision moves for every committed change, not only a transcript
// write: an agent finishing in the journal, and an unread agent read later.
func TestSubagentCallRevisionMovesWithEveryChange(t *testing.T) {
	db := newStore(t)
	write(t, db, "p", 1, 1)
	revision := func() int64 {
		t.Helper()
		calls, err := db.SubagentCalls(chatID("claude", "p"))
		if err != nil || len(calls) != 1 {
			t.Fatalf("calls = %+v, %v", calls, err)
		}
		return calls[0].Revision
	}
	w1 := session.Child{Key: "w1", CallID: "toolu_wf", Files: []session.ChildFile{childFile(t, 10)}}
	if _, err := db.SyncSubagents(parentMeta("p"), []session.Child{w1}, nil); err != nil {
		t.Fatal(err)
	}
	unread := revision()

	if _, err := db.SyncSubagents(parentMeta("p"), []session.Child{w1}, map[string][]agent.Event{"w1": {{Kind: agent.EventText, Text: "x"}}}); err != nil {
		t.Fatal(err)
	}
	read := revision()
	if read <= unread {
		t.Fatalf("reading an unread agent kept revision %d", read)
	}

	w1.Done = true
	if _, err := db.SyncSubagents(parentMeta("p"), []session.Child{w1}, nil); err != nil {
		t.Fatal(err)
	}
	if finished := revision(); finished <= read {
		t.Fatalf("a journal-only finish kept revision %d", finished)
	}
}

// A reopened cache hands out revisions past every subagent's, so a call's
// newest revision still moves when one of its agents writes again.
func TestSubagentRevisionsSurviveAReopen(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, db, "p", 1, 1)
	w1 := session.Child{Key: "w1", CallID: "toolu_wf"}
	w2 := session.Child{Key: "w2", CallID: "toolu_wf", Order: 1}
	for range 5 {
		w1.Result += "x"
		if _, err := db.SyncSubagents(parentMeta("p"), []session.Child{w1, w2}, nil); err != nil {
			t.Fatal(err)
		}
	}
	calls, _ := db.SubagentCalls(chatID("claude", "p"))
	before := calls[0].Revision
	db.Close()

	db, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	w2.Done = true
	if _, err := db.SyncSubagents(parentMeta("p"), []session.Child{w1, w2}, nil); err != nil {
		t.Fatal(err)
	}
	if calls, _ := db.SubagentCalls(chatID("claude", "p")); calls[0].Revision <= before {
		t.Fatalf("revision after reopen = %d, want past %d", calls[0].Revision, before)
	}
}
