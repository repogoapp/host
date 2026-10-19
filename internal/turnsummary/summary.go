// Package turnsummary writes the one summary of an agent's turn that the chat
// list, the voice agent and the in-app alert show: the recap the agent ended
// its last message with, else that message's opening.
package turnsummary

import (
	"regexp"
	"strings"
)

// MaxChars caps a summary, ellipsis included: room for a short recap, while
// a list row clips it at its own width.
const MaxChars = 600

// Text is the summary of an agent's final message: its recap, the last
// top-level blockquote when only whitespace or ui fences follow it, else its
// opening prose. Plain text on one line; "" for a message with no prose.
func Text(message string) string {
	blocks := parse(message)
	for i := len(blocks) - 1; i >= 0; i-- {
		b := blocks[i]
		if b.kind == fence && b.info == "ui" {
			continue
		}
		if b.kind == quote {
			return plain(b.lines)
		}
		break
	}
	var prose []string
	for _, b := range blocks {
		if b.kind != fence {
			prose = append(prose, b.lines...)
		}
	}
	return plain(prose)
}

type blockKind int

const (
	paragraph blockKind = iota
	quote
	fence
)

// block is a run of lines of one kind: a quote's lines without their `>`.
type block struct {
	kind  blockKind
	info  string
	lines []string
}

var (
	fenceOpen = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})\\s*(\\S*)")
	quoteLine = regexp.MustCompile(`^ {0,3}>\s?`)
)

// parse splits markdown into top-level blocks. A `>` inside a list item or
// a code block is that block's text, not a quote.
func parse(text string) []block {
	var blocks []block
	var open *block
	closing := ""
	for line := range strings.SplitSeq(text, "\n") {
		if closing != "" {
			if t := strings.TrimSpace(line); strings.HasPrefix(t, closing) && strings.Trim(t, closing[:1]) == "" {
				closing, open = "", nil
			}
			continue
		}
		if m := fenceOpen.FindStringSubmatch(line); m != nil {
			closing = m[1]
			blocks = append(blocks, block{kind: fence, info: m[2]})
			open = nil
			continue
		}
		if strings.TrimSpace(line) == "" {
			open = nil
			continue
		}
		kind := paragraph
		if loc := quoteLine.FindStringIndex(line); loc != nil {
			kind, line = quote, line[loc[1]:]
		}
		if open == nil || open.kind != kind {
			blocks = append(blocks, block{kind: kind})
			open = &blocks[len(blocks)-1]
		}
		open.lines = append(open.lines, line)
	}
	return blocks
}

var (
	image      = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	link       = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	lineMarker = regexp.MustCompile(`^\s*(#{1,6}\s+|>\s?|[-*+•]\s+|\d+[.)]\s+)+`)
	marks      = strings.NewReplacer("**", "", "__", "", "`", "")
)

// plain is lines as one line of prose: link labels kept, emphasis and code
// marks and each line's heading, list or quote marker dropped, then capped.
func plain(lines []string) string {
	words := make([]string, 0, len(lines))
	for _, line := range lines {
		line = lineMarker.ReplaceAllString(line, "")
		line = image.ReplaceAllString(line, "$1")
		line = link.ReplaceAllString(line, "$1")
		words = append(words, marks.Replace(line))
	}
	out := strings.Join(strings.Fields(strings.Join(words, " ")), " ")
	if runes := []rune(out); len(runes) > MaxChars {
		out = strings.TrimRight(string(runes[:MaxChars-1]), " ") + "…"
	}
	return out
}
