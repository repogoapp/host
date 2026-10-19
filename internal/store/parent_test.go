package store

import (
	"slices"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
)

func listIDs(t *testing.T, db *Store, q ChatQuery) []ChatID {
	t.Helper()
	page, err := db.Chats(q)
	if err != nil {
		t.Fatal(err)
	}
	var out []ChatID
	for _, c := range page.Chats {
		out = append(out, c.ID)
	}
	slices.Sort(out)
	return out
}

// A chat another chat's agent started is listed under its parent, not in the
// drawer, and search still finds it.
func TestAChildIsListedUnderItsParentOnly(t *testing.T) {
	db := newStore(t)
	write(t, db, "parent", 1000, 1)
	write(t, db, "child", 2000, 1)
	if err := db.SetChatParent("claude:child", "claude:parent"); err != nil {
		t.Fatal(err)
	}

	if got := listIDs(t, db, ChatQuery{}); !slices.Equal(got, []ChatID{"claude:parent"}) {
		t.Fatalf("drawer lists %v, want only the parent", got)
	}
	if got := listIDs(t, db, ChatQuery{Parent: "claude:parent"}); !slices.Equal(got, []ChatID{"claude:child"}) {
		t.Fatalf("the parent's children are %v", got)
	}
	if got := listIDs(t, db, ChatQuery{Search: "chat"}); !slices.Equal(got, []ChatID{"claude:child", "claude:parent"}) {
		t.Fatalf("search finds %v, want both", got)
	}
	c, err := db.Info("claude:child")
	if err != nil || c.ParentID != "claude:parent" {
		t.Fatalf("child row has parent %q (%v)", c.ParentID, err)
	}
}

// A marks row holding only a parent survives clearing another mark and a
// re-import, both of which drop rows with nothing set.
func TestAParentOnlyMarkSurvives(t *testing.T) {
	db := newStore(t)
	write(t, db, "child", 1000, 1)
	if err := db.SetChatParent("claude:child", "claude:parent"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetResolved("claude:child", true, time.UnixMilli(2000)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetResolved("claude:child", false, time.UnixMilli(3000)); err != nil {
		t.Fatal(err)
	}
	c := syncEvents(t, db, agent.KindClaude, "child", []agent.Event{
		{Kind: agent.EventUserMessage, Text: "again", At: 4000},
	})
	if c.ParentID != "claude:parent" {
		t.Fatalf("parent after an unresolve and a re-import: %q", c.ParentID)
	}
}

// Deleting a parent puts its children back in the drawer, with a new revision
// so every phone hears of it.
func TestDeletingAParentFreesItsChildren(t *testing.T) {
	db := newStore(t)
	write(t, db, "parent", 1000, 1)
	write(t, db, "child", 2000, 1)
	if err := db.SetChatParent("claude:child", "claude:parent"); err != nil {
		t.Fatal(err)
	}
	before, err := db.Info("claude:child")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Delete("claude:parent"); err != nil {
		t.Fatal(err)
	}
	after, err := db.Info("claude:child")
	if err != nil {
		t.Fatal(err)
	}
	if after.ParentID != "" || after.Revision <= before.Revision {
		t.Fatalf("child after its parent's delete: parent %q, revision %d (was %d)", after.ParentID, after.Revision, before.Revision)
	}
	if got := listIDs(t, db, ChatQuery{}); !slices.Equal(got, []ChatID{"claude:child"}) {
		t.Fatalf("drawer lists %v, want the freed child", got)
	}
	var rows int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM state.chat_marks`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("chat_marks holds %d rows, want 0 (%v)", rows, err)
	}
}
