package store

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
)

// syncChat writes one Claude chat with the given title and events.
func syncChat(t *testing.T, db *Store, id, title string, events ...agent.Event) {
	t.Helper()
	err := db.Sync(session.Meta{
		Agent: agent.KindClaude, ID: id, Title: title, Cwd: "/tmp/" + id,
		UpdatedAt: time.UnixMilli(1000), SizeBytes: int64(len(events)),
	}, events)
	if err != nil {
		t.Fatalf("sync %s: %v", id, err)
	}
}

func prompt(text string) agent.Event { return agent.Event{Kind: agent.EventUserMessage, Text: text} }
func reply(text string) agent.Event  { return agent.Event{Kind: agent.EventText, Text: text} }

// found is the ids a search returns, and fails the test on an error.
func found(t *testing.T, db *Store, search string) []ChatID {
	t.Helper()
	page, err := db.Chats(ChatQuery{Search: search})
	if err != nil {
		t.Fatalf("search %q: %v", search, err)
	}
	ids := []ChatID{}
	for _, c := range page.Chats {
		ids = append(ids, c.ID)
	}
	return ids
}

func searchRows(t *testing.T, db *Store) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM chat_search`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSearchFindsPromptsRepliesAndTitles(t *testing.T) {
	db := newStore(t)
	syncChat(t, db, "a", "Release notes", prompt("the sparkle updater fails"), reply("Fixed the appcast signature."))
	syncChat(t, db, "b", "Unrelated", prompt("rename the drawer"), reply("Renamed it."))

	for search, want := range map[string][]ChatID{
		"sparkle":   {"claude:a"}, // the user's prompt
		"appcast":   {"claude:a"}, // the agent's reply
		"release":   {"claude:a"}, // the title
		"drawer":    {"claude:b"},
		"quicksand": {},
	} {
		if got := found(t, db, search); !slices.Equal(got, want) {
			t.Errorf("search %q = %v, want %v", search, got, want)
		}
	}
}

func TestSearchMarksTheWordsAroundTheHit(t *testing.T) {
	db := newStore(t)
	syncChat(t, db, "a", "Release notes", prompt("the sparkle updater fails"))

	page, err := db.Chats(ChatQuery{Search: "sparkle"})
	if err != nil || len(page.Chats) != 1 {
		t.Fatalf("search = %+v, %v", page.Chats, err)
	}
	if got := page.Chats[0].Match; !strings.Contains(got, "⟪sparkle⟫") {
		t.Errorf("match = %q, want the word marked", got)
	}
	// Found by its title alone: the title is the reason, so no snippet.
	page, err = db.Chats(ChatQuery{Search: "release"})
	if err != nil || len(page.Chats) != 1 || page.Chats[0].Match != "" {
		t.Errorf("title-only search = %+v, %v; want one row with no match", page.Chats, err)
	}
	// A plain list carries no match.
	page, err = db.Chats(ChatQuery{})
	if err != nil || len(page.Chats) != 1 || page.Chats[0].Match != "" {
		t.Errorf("list = %+v, %v; want no match", page.Chats, err)
	}
}

// Tool output and reasoning are left out: a file dump or a thought would make
// nearly every chat match.
func TestSearchSkipsToolOutputAndReasoning(t *testing.T) {
	db := newStore(t)
	syncChat(t, db, "a", "Chat",
		agent.Event{Kind: agent.EventReasoning, Text: "pondering zeppelins"},
		agent.Event{Kind: agent.EventToolCall, Tool: &agent.ToolCall{Name: "Read", Output: "zeppelins everywhere"}})
	if got := found(t, db, "zeppelins"); len(got) != 0 {
		t.Errorf("search = %v, want nothing", got)
	}
}

func TestSearchNeedsEveryWordAnywhereInTheChat(t *testing.T) {
	db := newStore(t)
	syncChat(t, db, "a", "Chat", prompt("edit the queue"), reply("the tool call is queued"))

	if got := found(t, db, "queue tool"); !slices.Equal(got, []ChatID{"claude:a"}) {
		t.Errorf("words in two messages = %v, want the chat", got)
	}
	if got := found(t, db, "queue zebra"); len(got) != 0 {
		t.Errorf("one word missing = %v, want nothing", got)
	}
}

// Spoken queries: filler words drop, other forms of a word match, and the
// last word may be half typed.
func TestSearchMatchesHowPeopleAsk(t *testing.T) {
	db := newStore(t)
	syncChat(t, db, "a", "Chat", prompt("we edited the queue of tool-call actions"))

	for _, search := range []string{
		"can you find the chat about editing the queue for tool call queue actions",
		"tool-call",
		"editing",
		"queue act",
	} {
		if got := found(t, db, search); !slices.Equal(got, []ChatID{"claude:a"}) {
			t.Errorf("search %q = %v, want the chat", search, got)
		}
	}
}

func TestSearchTextIsNeverQuerySyntax(t *testing.T) {
	db := newStore(t)
	syncChat(t, db, "a", "Chat", prompt("drop table near x"))

	for _, search := range []string{`"; DROP TABLE x; NEAR( *`, "AND", "OR NOT", "-drop", `a"b`, "x*", "(near)"} {
		if _, err := db.Chats(ChatQuery{Search: search}); err != nil {
			t.Errorf("search %q: %v", search, err)
		}
	}
	if got := found(t, db, `"; DROP TABLE x; NEAR( *`); !slices.Equal(got, []ChatID{"claude:a"}) {
		t.Errorf("punctuated search = %v, want the chat by its words", got)
	}
	if got := found(t, db, "!!! ???"); len(got) != 0 {
		t.Errorf("no words = %v, want nothing", got)
	}
}

func TestSearchWords(t *testing.T) {
	for query, want := range map[string]string{
		"live activ":                          "live activ*",
		"tool-call queue":                     "tool call queue*",
		"Can you find the chat about Sparkle": "sparkle*",
		"the":                                 "the*",
		"fix ci":                              "fix ci",
		"!!!":                                 "",
		strings.Repeat("a", 250):              strings.Repeat("a", maxSearchChars) + "*",
	} {
		if got := searchMatch(query); got != want {
			t.Errorf("searchMatch(%.20q) = %q, want %q", query, got, want)
		}
	}
}

func TestResyncKeepsTheIndexCurrent(t *testing.T) {
	db := newStore(t)
	syncChat(t, db, "a", "Chat", prompt("alpha"))
	syncChat(t, db, "a", "Chat", prompt("alpha")) // unchanged: the row stays
	if got := found(t, db, "alpha"); !slices.Equal(got, []ChatID{"claude:a"}) {
		t.Fatalf("after an unchanged sync = %v, want the chat", got)
	}

	syncChat(t, db, "a", "Renamed", prompt("alpha"), reply("beta"))
	if got := found(t, db, "beta"); !slices.Equal(got, []ChatID{"claude:a"}) {
		t.Errorf("new reply = %v, want the chat", got)
	}
	if got := found(t, db, "renamed"); !slices.Equal(got, []ChatID{"claude:a"}) {
		t.Errorf("new title = %v, want the chat", got)
	}

	// A rewritten file is a newer one; the same stamp with fewer rows reads as stale.
	if err := db.Sync(session.Meta{Agent: agent.KindClaude, ID: "a", Title: "Renamed", Cwd: "/tmp/a",
		UpdatedAt: time.UnixMilli(2000), SizeBytes: 1}, []agent.Event{prompt("gamma")}); err != nil {
		t.Fatal(err)
	}
	if got := found(t, db, "alpha"); len(got) != 0 {
		t.Errorf("rewritten transcript still found by old text: %v", got)
	}
	if n := searchRows(t, db); n != 1 {
		t.Errorf("search rows = %d, want one per chat", n)
	}
}

func TestDeleteAndPruneDropTheSearchRow(t *testing.T) {
	db := newStore(t)
	syncChat(t, db, "a", "Chat", prompt("shared word"))
	syncChat(t, db, "b", "Chat", prompt("shared word"))
	syncChat(t, db, "c", "Chat", prompt("shared word"))

	if err := db.Delete("claude:a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.Prune(map[string]bool{Key(session.Meta{Agent: agent.KindClaude, ID: "c"}): true}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got := found(t, db, "shared"); !slices.Equal(got, []ChatID{"claude:c"}) {
		t.Errorf("search after delete and prune = %v, want only claude:c", got)
	}
	if n := searchRows(t, db); n != 1 {
		t.Errorf("search rows = %d, want 1", n)
	}
}

// The transcript's arrows step through the search results, not the whole list.
func TestNeighborsStayInsideASearch(t *testing.T) {
	db := newStore(t)
	for i, id := range []string{"a", "b", "c"} {
		text := "other"
		if id != "b" {
			text = "sparkle"
		}
		err := db.Sync(session.Meta{Agent: agent.KindClaude, ID: id, Title: "Chat", Cwd: "/tmp",
			UpdatedAt: time.UnixMilli(int64(1000 * (i + 1))), SizeBytes: 1}, []agent.Event{prompt(text)})
		if err != nil {
			t.Fatal(err)
		}
	}
	previous, next, err := db.Neighbors("claude:c", ChatQuery{Search: "sparkle"})
	if err != nil {
		t.Fatalf("neighbors: %v", err)
	}
	if previous != nil || next == nil || next.ID != "claude:a" {
		t.Errorf("neighbors of c = %v, %v; want nil and claude:a", previous, next)
	}
}

// Agents write curly quotes and dashes; the tokenizer splits only on ASCII
// punctuation, so the index holds them as ASCII and each word is findable.
func TestSearchSeesThroughTypographicPunctuation(t *testing.T) {
	db := newStore(t)
	syncChat(t, db, "a", "Chat", reply("the foo—bar split, “quoted” words’ end, CAFÉ ✅done"))

	for _, search := range []string{"foo bar", "foo—bar", "quoted", "words", "café", "CAFÉ", "done"} {
		if got := found(t, db, search); !slices.Equal(got, []ChatID{"claude:a"}) {
			t.Errorf("search %q = %v, want the chat", search, got)
		}
	}
	page, err := db.Chats(ChatQuery{Search: "quoted"})
	if err != nil || len(page.Chats) != 1 || !strings.Contains(page.Chats[0].Match, `"⟪quoted⟫"`) {
		t.Errorf("match = %+v, %v; want the word marked inside straight quotes", page.Chats, err)
	}
}

// A turn just sent makes the chat before its transcript exists; it is found by
// its title from then, and the transcript's text joins once it syncs.
func TestAChatIsFoundByItsTitleBeforeItsTranscript(t *testing.T) {
	db := newStore(t)
	started := time.UnixMilli(1000)
	observe := func() {
		t.Helper()
		err := db.SetStatus("claude:new", "/tmp/new", agent.ChatWorking, started,
			TurnObservation{Key: "k", StartedAt: &started, Prompt: "Fix the sparkle updater"})
		if err != nil {
			t.Fatalf("set status: %v", err)
		}
	}
	observe()
	if got := found(t, db, "sparkle"); !slices.Equal(got, []ChatID{"claude:new"}) {
		t.Fatalf("before the transcript = %v, want the chat by its title", got)
	}

	syncChat(t, db, "new", "Fix the sparkle updater", prompt("Fix the sparkle updater"), reply("The appcast is fixed."))
	observe() // a later status must not drop the transcript's text
	if got := found(t, db, "appcast"); !slices.Equal(got, []ChatID{"claude:new"}) {
		t.Errorf("after the transcript = %v, want the chat by its reply", got)
	}
	if n := searchRows(t, db); n != 1 {
		t.Errorf("search rows = %d, want 1", n)
	}
}

// A search pages like the list: the cursor keeps the search.
func TestSearchPages(t *testing.T) {
	db := newStore(t)
	for i, id := range []string{"a", "b", "c", "d"} {
		text := "sparkle"
		if id == "b" {
			text = "other"
		}
		err := db.Sync(session.Meta{Agent: agent.KindClaude, ID: id, Title: "Chat", Cwd: "/tmp",
			UpdatedAt: time.UnixMilli(int64(1000 * (i + 1))), SizeBytes: 1}, []agent.Event{prompt(text)})
		if err != nil {
			t.Fatal(err)
		}
	}
	first, err := db.Chats(ChatQuery{Search: "sparkle", Limit: 2})
	if err != nil || len(first.Chats) != 2 || first.NextCursor == "" {
		t.Fatalf("first page = %+v, %v", first, err)
	}
	second, err := db.Chats(ChatQuery{Search: "sparkle", Limit: 2, Cursor: first.NextCursor})
	if err != nil || len(second.Chats) != 1 || second.Chats[0].ID != "claude:a" {
		t.Errorf("second page = %+v, %v; want only claude:a", second.Chats, err)
	}
}
