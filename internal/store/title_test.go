package store

import (
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
)

func user(text string) agent.Event {
	return agent.Event{Kind: agent.EventUserMessage, Text: text}
}

func TestTitleForPrefersTheProviderTitle(t *testing.T) {
	meta := session.Meta{Title: "  Debug the relay  "}
	got := titleFor(meta, []agent.Event{user("something else entirely")})
	if got != "Debug the relay" {
		t.Errorf("titleFor = %q, want the provider's own title, whitespace collapsed", got)
	}
}

func TestTitleForFallsBackToTheFirstPrompt(t *testing.T) {
	events := []agent.Event{
		{Kind: agent.EventTurnStarted},
		user("Fix the scroll position on open please, it jumps to the top every time"),
		user("and also rename the button"),
	}
	got := titleFor(session.Meta{}, events)
	want := "Fix the scroll position on open please, it jumps to the top…"
	if got != want {
		t.Errorf("titleFor = %q, want %q", got, want)
	}
}

func TestTitleForSkipsEmptyPrompts(t *testing.T) {
	events := []agent.Event{user("   \n\t "), user("second one wins")}
	if got := titleFor(session.Meta{}, events); got != "second one wins" {
		t.Errorf("titleFor = %q, want the first prompt with prose in it", got)
	}
}

func TestTitleForIsEmptyWithoutAnyUserMessage(t *testing.T) {
	events := []agent.Event{{Kind: agent.EventText, Text: "the agent talking"}}
	if got := titleFor(session.Meta{}, events); got != "" {
		t.Errorf("titleFor = %q, want empty so the client can fall back", got)
	}
}

func TestOpeningWordsCollapsesNewlines(t *testing.T) {
	if got := openingWords("one\n\ntwo\tthree"); got != "one two three" {
		t.Errorf("openingWords = %q, want a single line", got)
	}
}

func TestOpeningWordsCapsLongWords(t *testing.T) {
	// Two "words", one of which is a path — the word count alone would let a
	// 200-character title through.
	got := openingWords("check " + strings.Repeat("a/very/long/path/", 12))
	if runes := []rune(got); len(runes) > titleMaxChars+1 {
		t.Errorf("openingWords = %d runes, want at most %d plus the ellipsis",
			len(runes), titleMaxChars)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("openingWords = %q, want a truncation marker", got)
	}
}

func TestOpeningWordsDropsTheStopBeforeTheEllipsis(t *testing.T) {
	got := openingWords("For my model name filter. It should match on the family, then the version")
	if want := "For my model name filter. It should match on the family, then…"; got != want {
		t.Errorf("openingWords = %q, want %q", got, want)
	}
}

func TestOpeningWordsKeepsAShortPromptWhole(t *testing.T) {
	if got := openingWords("ship it"); got != "ship it" {
		t.Errorf("openingWords = %q, want no ellipsis on a short prompt", got)
	}
}

func TestTitleForDropsAttachmentLinks(t *testing.T) {
	events := []agent.Event{user("Just call this in sync project tabs [@image-1.jpg](file:///Users/me/.repogo/attachments/a/image-1.jpg)")}
	if got := titleFor(session.Meta{}, events); got != "Just call this in sync project tabs" {
		t.Errorf("titleFor = %q, want the prompt without its attachment link", got)
	}
}

// A chat sent from the phone is titled at turn start, before any transcript
// line, so that path drops attachment links too.
func TestTurnStartTitleDropsAttachmentLinks(t *testing.T) {
	db := newStore(t)
	started := time.UnixMilli(1_000)
	sent := TurnObservation{Key: ManagerTurnPrefix + "t1", StartedAt: &started,
		Prompt: "How can I have pinned chats not show twice [@image-1.jpg](file:///Users/me/.repogo/attachments/a/image-1.jpg)"}
	if err := db.SetStatus("claude:new", "/w", agent.ChatWorking, started, sent); err != nil {
		t.Fatal(err)
	}
	chat, err := db.Info("claude:new")
	if err != nil {
		t.Fatal(err)
	}
	if chat.Title != "How can I have pinned chats not show twice" {
		t.Errorf("title = %q, want the prompt without its attachment link", chat.Title)
	}
}

func TestWithoutAttachmentsKeepsOrdinaryLinks(t *testing.T) {
	text := "see [@a.png](file:///tmp/a.png) and [docs](https://x.dev) [@b](https://x.dev/b.png)"
	if got := collapseSpace(withoutAttachments(text)); got != "see and [docs](https://x.dev)" {
		t.Errorf("withoutAttachments = %q", got)
	}
}

func TestPromptWordsShowsTheAnswersToAQuestion(t *testing.T) {
	text := "<send_user_message_question_reply>\n" +
		`[{"answer":"Plugged in","question":"Connect the iPhone?","questionItemId":"x"},{"answer":"Yes","question":"Record?","questionItemId":"y"}]` +
		"\n</send_user_message_question_reply>"
	if got := promptWords(text); got != "Plugged in · Yes" {
		t.Errorf("promptWords = %q, want the answers", got)
	}
}
