package store

import (
	"strings"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
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
