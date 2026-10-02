package store

import (
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
)

func TestLastUserMessagePreviewFollowsSavedTranscript(t *testing.T) {
	db := newStore(t)
	meta := session.Meta{Agent: agent.KindClaude, ID: "preview", UpdatedAt: time.UnixMilli(100)}
	events := []agent.Event{user("Original request"), user("**Latest**\nrequest"), {Kind: agent.EventText, Text: "Assistant reply"}}
	var epoch string
	var rev int64
	for _, step := range []struct {
		name   string
		events []agent.Event
		want   string
	}{
		{"latest user, not assistant", events, "Latest request"},
		{"new prompt while working", append(append([]agent.Event{}, events...), user(strings.Repeat("界", 250))), strings.Repeat("界", 240) + "…"},
		{"empty latest prompt does not show an older one", append(append([]agent.Event{}, events...), user("")), ""},
		{"rewritten transcript clears preview", []agent.Event{{Kind: agent.EventText, Text: "Assistant only"}}, ""},
	} {
		t.Run(step.name, func(t *testing.T) {
			meta.UpdatedAt = meta.UpdatedAt.Add(time.Second)
			if err := db.Sync(meta, step.events); err != nil {
				t.Fatal(err)
			}
			row, err := db.Info("claude:preview")
			if err != nil {
				t.Fatal(err)
			}
			if row.LastUserMessagePreview != step.want {
				t.Fatalf("info preview = %q, want %q", row.LastUserMessagePreview, step.want)
			}
			page, err := db.Chats(ChatQuery{})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Chats) != 1 || page.Chats[0].LastUserMessagePreview != step.want {
				t.Fatalf("list = %+v", page.Chats)
			}
			pull, err := db.Pull(PullRequest{Family: "chats", Epoch: epoch, Since: rev}, "host")
			if err != nil {
				t.Fatal(err)
			}
			if len(pull.Upsert) != 1 {
				t.Fatalf("upserts = %d, want 1", len(pull.Upsert))
			}
			mirrored := pull.Upsert[0]
			if mirrored.LastUserMessagePreview != step.want {
				t.Fatalf("mirror preview = %q, want %q", mirrored.LastUserMessagePreview, step.want)
			}
			epoch, rev = pull.Epoch, pull.Rev
		})
	}
}
