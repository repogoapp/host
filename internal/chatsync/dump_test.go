package chatsync

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/chatwire"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
)

var (
	dumpPages   = flag.String("dump-pages", "", "write the chats.messages pages of local chats to this directory")
	dumpLarge   = flag.Int("dump-large", 6, "the largest chats to dump per agent")
	dumpTypical = flag.Int("dump-typical", 6, "recent chats of 50+ rows to dump per agent")
)

// The phone's opening and scroll-up page sizes (`TranscriptStore+History`).
const (
	dumpTailLimit  = 40
	dumpOlderLimit = 120
)

// TestDumpPages writes, for a sample of local chats, the chats.messages pages a
// phone reads opening each and scrolling to its top, for
// the iOS transcript bench. They hold chat content: keep them out of git.
func TestDumpPages(t *testing.T) {
	if *dumpPages == "" {
		t.Skip("pass -dump-pages DIR to dump local chat history")
	}
	if err := os.MkdirAll(*dumpPages, 0700); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	claudeAgent, codexAgent := claude.New(agent.Dependencies{}), codex.New(agent.Dependencies{})
	// The pages as the phone receives them, not as the store holds them.
	wire := chatwire.New([]chatwire.Labeler{claudeAgent, codexAgent})
	for _, provider := range []session.Provider{claudeAgent.Sessions(), codexAgent.Sessions()} {
		db, err := store.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		stats := New(session.NewStore(provider), db, logger).once(context.Background(), 64)
		chats := allChats(t, db)
		t.Logf("synced %d of %d sessions, %d rows", stats.synced, stats.total, stats.events)
		for _, chat := range sample(chats) {
			name := fmt.Sprintf("%s-%05d-%s.jsonl", chat.Agent, chat.EventCount, shortID(chat.ID))
			pages, rows := writePages(t, db, wire, chat.ID, filepath.Join(*dumpPages, name))
			t.Logf("%s: %d pages, %d rows", name, pages, rows)
		}
		db.Close()
	}
}

func allChats(t *testing.T, db *store.Store) []store.Chat {
	var out []store.Chat
	q := store.ChatQuery{Limit: 500}
	for {
		page, err := db.Chats(q)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, page.Chats...)
		if page.NextCursor == "" {
			return out
		}
		q.Cursor = page.NextCursor
	}
}

// sample is the largest chats, then the most recent ordinary ones not already
// taken; `chats` arrives most recent first.
func sample(chats []store.Chat) []store.Chat {
	bySize := slices.Clone(chats)
	slices.SortFunc(bySize, func(a, b store.Chat) int { return cmp.Compare(b.EventCount, a.EventCount) })
	picked := bySize[:min(*dumpLarge, len(bySize))]
	typical := 0
	for _, chat := range chats {
		if typical == *dumpTypical {
			break
		}
		if chat.EventCount >= 50 && !slices.ContainsFunc(picked, func(c store.Chat) bool { return c.ID == chat.ID }) {
			picked = append(picked, chat)
			typical++
		}
	}
	return picked
}

func writePages(t *testing.T, db *store.Store, wire *chatwire.Builder, id store.ChatID, path string) (pages, rows int) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	page, err := db.Tail(id, dumpTailLimit)
	for {
		if err != nil {
			t.Fatal(err)
		}
		page.HostID = "bench"
		if err := enc.Encode(wire.Page(page)); err != nil {
			t.Fatal(err)
		}
		pages++
		rows += len(page.Events)
		if !page.HasBefore {
			return pages, rows
		}
		page, err = db.Before(id, page.FirstIdx, dumpOlderLimit)
	}
}

func shortID(id store.ChatID) string {
	s := string(id)
	return s[max(0, len(s)-8):]
}
