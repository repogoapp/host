package store

import (
	"encoding/json"
	"strings"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
)

const (
	// More than a phone row shows on one line: the row elides at its own
	// width, and a wider screen or the chat header gets the rest.
	titleWords = 12

	// A hard ceiling as well as a word count, because one "word" can be a URL
	// or an absolute path — `strings.Fields` would hand back a 200-character
	// title with five words in it.
	titleMaxChars = 80
)

// titleFor is the provider's own title, else the opening words of the first
// user message. Derived on the host at sync so `chats.list` can order on it;
// first message, never the latest, or the title renames itself every turn.
func titleFor(meta session.Meta, events []agent.Event) string {
	if t := collapseSpace(meta.Title); t != "" {
		return t
	}
	for _, e := range events {
		if e.Kind != agent.EventUserMessage {
			continue
		}
		// Already prose: the parser strips command wrappers and reminder blocks before
		// an event is produced.
		if t := promptTitle(e.Text); t != "" {
			return t
		}
	}
	return ""
}

// promptTitle is a prompt's title: its words, without attachment links.
func promptTitle(text string) string { return openingWords(promptWords(text)) }

// openingWords reduces a prompt to its first few words on one line.
func openingWords(text string) string {
	// Fields, not Split: it collapses runs of whitespace and drops newlines, so
	// a prompt pasted over six lines becomes one.
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	truncated := len(words) > titleWords
	if truncated {
		words = words[:titleWords]
	}
	out := strings.Join(words, " ")

	// Runes, not bytes: cutting a multi-byte character in half produces a title
	// that renders as a replacement glyph.
	if runes := []rune(out); len(runes) > titleMaxChars {
		out = strings.TrimRight(string(runes[:titleMaxChars]), " ")
		truncated = true
	}
	if truncated {
		// "filter.…" reads as a typo; the ellipsis stands in for the stop.
		out = strings.TrimRight(out, ".,;:!?") + "…"
	}
	return out
}

// promptWords is what a title or preview shows of a prompt: the answers in a
// question reply's envelope, else the prompt without its attachment links.
func promptWords(text string) string {
	if answers := questionAnswers(text); answers != "" {
		return answers
	}
	return withoutAttachments(text)
}

// questionAnswers reads the envelope a client wraps answers to an agent's
// questions in; empty for any other text.
func questionAnswers(text string) string {
	const open, close = "<send_user_message_question_reply>", "</send_user_message_question_reply>"
	body, ok := strings.CutPrefix(strings.TrimSpace(text), open)
	if body, ok = strings.CutSuffix(body, close); !ok {
		return ""
	}
	var replies []struct {
		Answer string `json:"answer"`
	}
	if json.Unmarshal([]byte(body), &replies) != nil {
		return ""
	}
	var answers []string
	for _, r := range replies {
		if a := strings.TrimSpace(r.Answer); a != "" {
			answers = append(answers, a)
		}
	}
	return strings.Join(answers, " · ")
}

// withoutAttachments drops the `[@name](file:///…)` links the bridges write
// for each attachment (`https://` for a v1 image).
func withoutAttachments(text string) string {
	var out strings.Builder
	rest := text
	for {
		open := strings.Index(rest, "[@")
		if open < 0 {
			break
		}
		name, after, ok := strings.Cut(rest[open+2:], "](")
		target, tail, closed := strings.Cut(after, ")")
		if !ok || !closed {
			break
		}
		out.WriteString(rest[:open])
		isLink := strings.HasPrefix(target, "file:///") || strings.HasPrefix(target, "https://")
		if name == "" || strings.Contains(name, "\n") || !isLink {
			out.WriteString(rest[open : len(rest)-len(tail)])
		}
		rest = tail
	}
	out.WriteString(rest)
	return out.String()
}

func collapseSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// replyMaxChars is enough for a two-line preview on a phone; the rest is a
// tap away and would only make every list row heavier.
const replyMaxChars = 240

// lastReply is the opening of the agent's newest text, the way a mail client
// previews a message. Both providers write assistant text only when a message
// completes, so this is always a whole thought, never a half-streamed one.
func lastReply(events []agent.Event) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind != agent.EventText {
			continue
		}
		if t := previewOf(events[i].Text); t != "" {
			return t
		}
	}
	return ""
}

// previewOf flattens markdown prose to one line: emphasis and code marks are
// noise at preview size, and a heading or list marker at the start reads as
// a stray symbol rather than structure.
func previewOf(text string) string {
	text = strings.NewReplacer("**", "", "__", "", "`", "").Replace(text)
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimLeft(line, " \t#>-*•")
		if line != "" {
			lines = append(lines, line)
		}
	}
	out := collapseSpace(strings.Join(lines, " "))
	if runes := []rune(out); len(runes) > replyMaxChars {
		out = strings.TrimRight(string(runes[:replyMaxChars]), " ") + "…"
	}
	return out
}
