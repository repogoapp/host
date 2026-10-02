package store

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// write puts one session with n text events into the cache.
func write(t *testing.T, db *Store, id string, updatedAt int64, n int) {
	t.Helper()
	events := make([]agent.Event, n)
	for i := range events {
		events[i] = agent.Event{Kind: agent.EventText, Text: fmt.Sprintf("m%d", i)}
	}
	err := db.Sync(session.Meta{
		Agent:     agent.KindClaude,
		ID:        id,
		Title:     "chat " + id,
		Cwd:       "/tmp/" + id,
		UpdatedAt: time.UnixMilli(updatedAt),
		SizeBytes: int64(n),
	}, events)
	if err != nil {
		t.Fatalf("sync %s: %v", id, err)
	}
}

func TestChatsAreNewestFirstAndPageByTime(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 100, 1)
	write(t, db, "b", 300, 1)
	write(t, db, "c", 200, 1)

	live := false
	page, err := db.Chats(ChatQuery{Limit: 2, Resolved: &live})
	if err != nil {
		t.Fatalf("chats: %v", err)
	}
	first := page.Chats
	if len(first) != 2 || first[0].ID != "claude:b" || first[1].ID != "claude:c" {
		t.Fatalf("wrong first page: %+v", first)
	}
	if page.NextCursor == "" {
		t.Fatal("full page carried no cursor")
	}

	// Scoped to one project: the sidebar picks a folder and wants that folder's
	// conversations, not everything on the machine.
	scopedPage, err := db.Chats(ChatQuery{Limit: 10, Cwd: "/tmp/a", Resolved: &live})
	if err != nil {
		t.Fatalf("chats by cwd: %v", err)
	}
	if scoped := scopedPage.Chats; len(scoped) != 1 || scoped[0].ID != "claude:a" {
		t.Fatalf("cwd filter returned %+v", scoped)
	}

	// The cursor is the last row's sort key, not an offset — a chat changing
	// mid-scroll must not make the next page skip or repeat.
	nextPage, err := db.Chats(ChatQuery{Limit: 2, Cursor: page.NextCursor, Resolved: &live})
	if err != nil {
		t.Fatalf("chats page 2: %v", err)
	}
	if next := nextPage.Chats; len(next) != 1 || next[0].ID != "claude:a" || nextPage.NextCursor != "" {
		t.Fatalf("wrong second page: %+v cursor=%q", next, nextPage.NextCursor)
	}

	// Title order, ascending, both piles: the inbox's other reading of the same rows.
	byTitle, err := db.Chats(ChatQuery{Limit: 10, Sort: SortTitle, Order: OrderAsc})
	if err != nil {
		t.Fatalf("chats by title: %v", err)
	}
	if len(byTitle.Chats) != 3 {
		t.Fatalf("title sort returned %d rows", len(byTitle.Chats))
	}
	for i := 1; i < len(byTitle.Chats); i++ {
		if byTitle.Chats[i-1].Title > byTitle.Chats[i].Title {
			t.Fatalf("title sort out of order: %+v", byTitle.Chats)
		}
	}
}

func TestChatsNarrowToOneAgent(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 100, 1)
	err := db.Sync(session.Meta{
		Agent: agent.KindCodex, ID: "b", Title: "chat b", Cwd: "/tmp/b",
		UpdatedAt: time.UnixMilli(200), SizeBytes: 1,
	}, []agent.Event{{Kind: agent.EventText, Text: "m0"}})
	if err != nil {
		t.Fatalf("sync b: %v", err)
	}

	for _, tc := range []struct {
		agent string
		want  []ChatID
	}{
		{"", []ChatID{"codex:b", "claude:a"}},
		{"claude", []ChatID{"claude:a"}},
		{"codex", []ChatID{"codex:b"}},
	} {
		page, err := db.Chats(ChatQuery{Limit: 10, Agent: tc.agent})
		if err != nil {
			t.Fatalf("chats agent=%q: %v", tc.agent, err)
		}
		var got []ChatID
		for _, c := range page.Chats {
			got = append(got, c.ID)
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Fatalf("agent=%q returned %v, want %v", tc.agent, got, tc.want)
		}
	}
}

// Created order holds a chat where it began, however recently it was active,
// and pages by that key like the others.
func TestChatsSortByCreationAndPage(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 100, 1)
	write(t, db, "b", 300, 1)
	write(t, db, "c", 200, 1)
	write(t, db, "a", 400, 2)

	page, err := db.Chats(ChatQuery{Limit: 2, Sort: SortCreated})
	if err != nil {
		t.Fatalf("chats by created: %v", err)
	}
	if got := page.Chats; len(got) != 2 || got[0].ID != "claude:b" || got[1].ID != "claude:c" {
		t.Fatalf("wrong first page: %+v", got)
	}
	next, err := db.Chats(ChatQuery{Limit: 2, Sort: SortCreated, Cursor: page.NextCursor})
	if err != nil {
		t.Fatalf("chats by created, page 2: %v", err)
	}
	if got := next.Chats; len(got) != 1 || got[0].ID != "claude:a" || next.NextCursor != "" {
		t.Fatalf("wrong second page: %+v cursor=%q", got, next.NextCursor)
	}
}

func TestNeighborsFollowTheListOrder(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 100, 1)
	write(t, db, "b", 300, 1)
	write(t, db, "c", 200, 1)

	// Newest first: b, c, a. The middle row has one on each side.
	live := false
	previous, next, err := db.Neighbors("claude:c", ChatQuery{Resolved: &live})
	if err != nil {
		t.Fatalf("neighbors: %v", err)
	}
	if previous == nil || previous.ID != "claude:b" || next == nil || next.ID != "claude:a" {
		t.Fatalf("wrong neighbours of c: previous=%+v next=%+v", previous, next)
	}

	// The top row has nothing above it and the bottom nothing below.
	if previous, next, _ = db.Neighbors("claude:b", ChatQuery{Resolved: &live}); previous != nil || next == nil || next.ID != "claude:c" {
		t.Fatalf("wrong neighbours of b: previous=%+v next=%+v", previous, next)
	}
	if previous, next, _ = db.Neighbors("claude:a", ChatQuery{Resolved: &live}); previous == nil || previous.ID != "claude:c" || next != nil {
		t.Fatalf("wrong neighbours of a: previous=%+v next=%+v", previous, next)
	}

	// Ascending by title (chat a, chat b, chat c): the same rows, read the
	// other way, so the arrows follow whatever order the list showed.
	if previous, next, _ = db.Neighbors("claude:b", ChatQuery{Sort: SortTitle, Order: OrderAsc}); previous == nil || previous.ID != "claude:a" || next == nil || next.ID != "claude:c" {
		t.Fatalf("wrong title-order neighbours of b: previous=%+v next=%+v", previous, next)
	}

	// Scoped to one project, the chat is alone.
	if previous, next, _ = db.Neighbors("claude:b", ChatQuery{Cwd: "/tmp/b"}); previous != nil || next != nil {
		t.Fatalf("scoped neighbours: previous=%+v next=%+v", previous, next)
	}

	if _, _, err := db.Neighbors("claude:missing", ChatQuery{}); err == nil {
		t.Fatal("neighbours of a missing chat did not fail")
	}
}

func TestMessagesResumeFromCursor(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 100, 10)

	page, err := db.Messages("claude:a", 0, 4)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(page.Events) != 4 || page.Events[0].Idx != 0 {
		t.Fatalf("first page wrong: %+v", page.Events)
	}
	if page.NextIdx != 4 {
		t.Errorf("next_idx = %d, want 4", page.NextIdx)
	}
	if page.EventCount != 10 {
		t.Errorf("event_count = %d, want 10", page.EventCount)
	}

	rest, err := db.Messages("claude:a", page.NextIdx, 100)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if len(rest.Events) != 6 || rest.Events[0].Idx != 4 {
		t.Fatalf("second page wrong: %+v", rest.Events)
	}
	if rest.NextIdx != rest.EventCount {
		t.Errorf("caught up, but next_idx %d != event_count %d", rest.NextIdx, rest.EventCount)
	}
}

// Re-requesting a page must return the same rows. A duplicate delivery over a
// flaky link is normal, and it has to be harmless.
func TestMessagePagesAreIdempotent(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 100, 5)

	first, _ := db.Messages("claude:a", 1, 3)
	again, _ := db.Messages("claude:a", 1, 3)

	if len(first.Events) != len(again.Events) {
		t.Fatalf("lengths differ: %d vs %d", len(first.Events), len(again.Events))
	}
	for i := range first.Events {
		if first.Events[i] != again.Events[i] {
			t.Fatalf("row %d differs between identical requests", i)
		}
	}
}

// The guard that stops a client splicing new history onto stale rows: a
// wholesale replace bumps `generation`, and a client holding the old one must
// notice and restart rather than appending.
func TestGenerationBumpsWhenHistoryIsRebuilt(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 100, 5)

	before, err := db.Messages("claude:a", 0, 100)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}

	// The provider rewrote the file: same session, different history.
	write(t, db, "a", 200, 3)

	after, err := db.Messages("claude:a", 0, 100)
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	if after.Generation == before.Generation {
		t.Fatal("history was replaced but generation did not move — a client would append onto stale rows")
	}
	if after.EventCount != 3 || len(after.Events) != 3 {
		t.Errorf("stale rows survived the replace: %d events", len(after.Events))
	}
}

func TestUnknownChatIsAnError(t *testing.T) {
	db := newStore(t)
	if _, err := db.Messages("claude:nope", 0, 10); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown chat: %v, want not found", err)
	}
	// A malformed id is the caller's mistake, not a chat that happens to be missing.
	if _, err := db.Messages("no-colon", 0, 10); !errors.Is(err, ErrInvalid) {
		t.Errorf("malformed id: %v, want invalid", err)
	}
}

func TestTailReturnsTheEnd(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 100, 20)

	page, err := db.Tail("claude:a", 5)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if len(page.Events) != 5 {
		t.Fatalf("want 5 events, got %d", len(page.Events))
	}
	// Ascending even though selected descending — a client renders oldest-first
	// no matter which way it paged.
	if page.Events[0].Idx != 15 || page.Events[4].Idx != 19 {
		t.Fatalf("wrong slice: %d…%d", page.Events[0].Idx, page.Events[4].Idx)
	}
	if page.NextIdx != 20 {
		t.Errorf("next_idx = %d, want 20 (caught up)", page.NextIdx)
	}
	if page.FirstIdx != 15 || !page.HasBefore {
		t.Errorf("first_idx = %d has_before = %v", page.FirstIdx, page.HasBefore)
	}
}

func TestBeforeWalksUpwardAndStopsAtZero(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 100, 12)

	tail, _ := db.Tail("claude:a", 5) // 7..11
	older, err := db.Before("claude:a", tail.FirstIdx, 5)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if len(older.Events) != 5 || older.Events[0].Idx != 2 || older.Events[4].Idx != 6 {
		t.Fatalf("wrong older page: %+v", older.Events)
	}
	if !older.HasBefore {
		t.Error("index 0 is not on screen yet, has_before should be true")
	}

	top, err := db.Before("claude:a", older.FirstIdx, 5)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if len(top.Events) != 2 || top.Events[0].Idx != 0 {
		t.Fatalf("want the first two events, got %+v", top.Events)
	}
	if top.HasBefore {
		t.Error("reached index 0 but still reports more above")
	}

	// Asking again from the top must not silently wrap to the tail.
	none, err := db.Before("claude:a", 0, 5)
	if err != nil {
		t.Fatalf("before 0: %v", err)
	}
	if len(none.Events) != 0 || none.HasBefore {
		t.Errorf("before(0) returned %d events", len(none.Events))
	}

	// A negative cursor is the dangerous one: internally it means "from the
	// end", so without a guard scrolling up past the top would jump the reader
	// back to the newest message.
	negative, err := db.Before("claude:a", -1, 5)
	if err != nil {
		t.Fatalf("before -1: %v", err)
	}
	if len(negative.Events) != 0 {
		t.Errorf("before(-1) returned the tail: %d events", len(negative.Events))
	}
}

// A page cut mid-reply leaves the client a reply it cannot time until the next
// page up lands, so backward pages widen to open on the reply's prompt.
func TestBackwardPagesOpenOnATurn(t *testing.T) {
	db := newStore(t)
	syncEvents(t, db, agent.KindCodex, "a", []agent.Event{
		{Kind: agent.EventUserMessage, Text: "one", At: 1_000}, // 0
		{Kind: agent.EventTurnStarted, TurnID: "t1", At: 1_000},
		{Kind: agent.EventText, Text: "a", At: 2_000},
		{Kind: agent.EventTurnFinished, TurnID: "t1", At: 3_000},
		{Kind: agent.EventUserMessage, Text: "two", At: 5_000}, // 4
		{Kind: agent.EventTurnStarted, TurnID: "t2", At: 5_000},
		{Kind: agent.EventText, Text: "b", At: 6_000},
		{Kind: agent.EventText, Text: "c", At: 7_000},
		{Kind: agent.EventTurnFinished, TurnID: "t2", At: 8_000}, // 8
	})

	// The last 2 rows are the tail of t2; its turn_started (5) comes along.
	tail, err := db.Tail("codex:a", 2)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	if tail.FirstIdx != 5 || len(tail.Events) != 4 || tail.NextIdx != 9 {
		t.Fatalf("tail opens at %d with %d rows, want 5 with 4", tail.FirstIdx, len(tail.Events))
	}

	// A page already on a boundary is left alone.
	older, err := db.Before("codex:a", tail.FirstIdx, 1)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if older.FirstIdx != 4 || len(older.Events) != 1 {
		t.Fatalf("older opens at %d with %d rows, want 4 with 1", older.FirstIdx, len(older.Events))
	}

	// Mid-reply: 2..3 widen back to t1's turn_started.
	top, err := db.Before("codex:a", older.FirstIdx, 2)
	if err != nil {
		t.Fatalf("before: %v", err)
	}
	if top.FirstIdx != 1 || len(top.Events) != 3 || !top.HasBefore {
		t.Fatalf("top opens at %d with %d rows, want 1 with 3", top.FirstIdx, len(top.Events))
	}
}

// The invariant the UI depends on: paging upward from the tail and stitching
// the pages together reproduces the transcript in order, oldest first. The
// newest message is last, so it renders at the bottom.
func TestPagingUpwardReassemblesTheTranscriptInOrder(t *testing.T) {
	db := newStore(t)
	const total = 47
	write(t, db, "a", 100, total)

	page, err := db.Tail("claude:a", 10)
	if err != nil {
		t.Fatalf("tail: %v", err)
	}
	assembled := append([]Message(nil), page.Events...)

	for page.HasBefore {
		page, err = db.Before("claude:a", page.FirstIdx, 10)
		if err != nil {
			t.Fatalf("before: %v", err)
		}
		// Older pages go in FRONT, which is what the client does when it
		// prepends while the reader scrolls up.
		assembled = append(append([]Message(nil), page.Events...), assembled...)
	}

	if len(assembled) != total {
		t.Fatalf("assembled %d events, want %d", len(assembled), total)
	}
	for i, m := range assembled {
		if m.Idx != i {
			t.Fatalf("event %d is out of order: idx %d", i, m.Idx)
		}
	}
	if assembled[0].Idx != 0 {
		t.Error("the first message is not at the top")
	}
	if assembled[len(assembled)-1].Idx != total-1 {
		t.Error("the newest message is not at the bottom")
	}
}

// An append must not bump generation: the client drops everything rendered
// when it changes, so a streaming agent would rebuild the transcript and lose
// the reader's scroll several times a second.
func TestAppendDoesNotBumpGeneration(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 100, 5)

	before, err := db.Messages("claude:a", 0, 100)
	if err != nil {
		t.Fatal(err)
	}

	// The same conversation, three events longer — what every live append is.
	write(t, db, "a", 200, 8)

	after, err := db.Messages("claude:a", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != before.Generation {
		t.Fatalf("generation moved on a pure append: %d → %d — clients will wipe their transcript",
			before.Generation, after.Generation)
	}
	if after.EventCount != 8 {
		t.Errorf("event_count = %d, want 8", after.EventCount)
	}
}

// A re-sync with no change at all must not bump either — chatlive re-reads the
// file on every watcher tick, including ones that changed nothing we record.
func TestIdenticalResyncDoesNotBumpGeneration(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 100, 6)
	before, _ := db.Messages("claude:a", 0, 100)

	write(t, db, "a", 100, 6)
	after, _ := db.Messages("claude:a", 0, 100)

	if after.Generation != before.Generation {
		t.Errorf("generation moved on an identical resync: %d → %d",
			before.Generation, after.Generation)
	}
}

// The sweep and a chat's flush parse the same file apart, so the one that read
// it first can write last. Its shorter rows must not replace the newer ones:
// that read as a rewrite and made every phone reload the chat.
func TestAnOlderReadDoesNotReplaceANewerOne(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 100, 5)
	write(t, db, "a", 200, 8)
	before, _ := db.Messages("claude:a", 0, 100)

	write(t, db, "a", 100, 5)
	after, _ := db.Messages("claude:a", 0, 100)
	if after.Generation != before.Generation || after.EventCount != 8 {
		t.Errorf("an older read replaced a newer one: generation %d → %d, %d events",
			before.Generation, after.Generation, after.EventCount)
	}

	// Read at the same modification time, before the rest of the line landed.
	write(t, db, "a", 200, 8)
	err := db.Sync(session.Meta{Agent: agent.KindClaude, ID: "a", Cwd: "/tmp/a", UpdatedAt: time.UnixMilli(200), SizeBytes: 8},
		make([]agent.Event, 6))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := db.Messages("claude:a", 0, 100); got.Generation != before.Generation || got.EventCount != 8 {
		t.Errorf("a shorter read of the same file replaced it: generation %d, %d events", got.Generation, got.EventCount)
	}
}

// A page has to be actionable on its own: running a turn or showing a diff
// needs its folder, and a push, deep link or reconnect has no list behind it.
func TestEveryReadPathCarriesTheProjectAddress(t *testing.T) {
	db := newStore(t)
	write(t, db, "a", 100, 8)

	forward, err := db.Messages("claude:a", 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	tail, err := db.Tail("claude:a", 4)
	if err != nil {
		t.Fatal(err)
	}
	before, err := db.Before("claude:a", 4, 4)
	if err != nil {
		t.Fatal(err)
	}

	for name, page := range map[string]Page{"messages": forward, "tail": tail, "before": before} {
		if page.Cwd != "/tmp/a" {
			t.Errorf("%s: cwd = %q, want /tmp/a", name, page.Cwd)
		}
		if page.Agent != "claude" {
			t.Errorf("%s: agent = %q, want claude", name, page.Agent)
		}
	}
}

// A limit past the cap is the cap, not the default: asking for more must not
// return fewer than asking for exactly the maximum.
func TestOverMaxLimitsClampToTheMaximum(t *testing.T) {
	db := newStore(t)
	write(t, db, "big", 1000, maxEvents+10)
	page, err := db.Messages("claude:big", 0, maxEvents*5)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != maxEvents {
		t.Fatalf("got %d events, want the %d maximum", len(page.Events), maxEvents)
	}
	if q, _ := (ChatQuery{Limit: maxChats * 5}).withDefaults(); q.Limit != maxChats {
		t.Fatalf("chat limit = %d, want %d", q.Limit, maxChats)
	}
}

func TestChatIDSessionID(t *testing.T) {
	for id, want := range map[ChatID]string{"claude:s-1": "s-1", "codex:a:b": "a:b", "nocolon": "", "claude:": ""} {
		if got := id.SessionID(); got != want {
			t.Errorf("%q.SessionID() = %q, want %q", id, got, want)
		}
	}
}

func TestSearchIsTrimmed(t *testing.T) {
	db := newStore(t)
	write(t, db, "demo", 1000, 1)
	page, err := db.Chats(ChatQuery{Search: "  chat demo \n"})
	if err != nil || len(page.Chats) != 1 {
		t.Fatalf("padded search = %+v, %v; want the one chat", page.Chats, err)
	}
}

// A tool sheet reads its calls' call and result rows by call id, in order,
// and nothing else of the chat.
func TestToolCallsByCallID(t *testing.T) {
	db := newStore(t)
	events := []agent.Event{
		{Kind: agent.EventUserMessage, Text: "go"},
		{Kind: agent.EventToolCall, Tool: &agent.ToolCall{CallID: "a", Name: "Read"}},
		{Kind: agent.EventToolCall, Tool: &agent.ToolCall{CallID: "b", Name: "Bash"}},
		{Kind: agent.EventText, Text: "reading"},
		{Kind: agent.EventToolResult, Tool: &agent.ToolCall{CallID: "a", Output: "module x"}},
		{Kind: agent.EventToolResult, Tool: &agent.ToolCall{CallID: "b", Output: "ok"}},
	}
	if err := db.Sync(session.Meta{Agent: agent.KindClaude, ID: "s", Cwd: "/w", UpdatedAt: time.UnixMilli(1)}, events); err != nil {
		t.Fatal(err)
	}

	rows, err := db.ToolCalls("claude:s", []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Idx != 1 || rows[1].Idx != 4 {
		t.Fatalf("rows %+v, want the call at 1 and the result at 4", rows)
	}
	if rows, _ := db.ToolCalls("claude:s", []string{"missing"}); len(rows) != 0 {
		t.Errorf("unknown call id: %+v", rows)
	}
	if _, err := db.ToolCalls("claude:nope", []string{"a"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown chat: %v", err)
	}
}
