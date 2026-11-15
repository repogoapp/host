package searchrank

import (
	"fmt"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
	"github.com/repogoapp/host/internal/store"
)

// Synthetic history keeps the ranking benchmark independent of local sessions.
func BenchmarkSearch(b *testing.B) {
	db, err := store.Open(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	for i := range 2000 {
		title := "Transport notes"
		if i%10 == 0 {
			title = "Investigate cache repair"
		}
		if i == 0 {
			title = "Cache repair"
		}
		err := db.Sync(session.Meta{Agent: agent.KindClaude, ID: fmt.Sprintf("chat-%04d", i),
			Title: title, Cwd: "/project", UpdatedAt: time.UnixMilli(int64(i + 1)), SizeBytes: 1},
			[]agent.Event{{Kind: agent.EventUserMessage, Text: "Investigate cache repair and connection recovery"}})
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for b.Loop() {
		if _, err := db.Chats(store.ChatQuery{Search: "cache repair", Limit: 50}); err != nil {
			b.Fatal(err)
		}
	}
}
