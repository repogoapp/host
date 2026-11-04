// Package turnsummary extracts the recap for chat lists, voice and alerts,
// falling back to the final message's opening. Strip and Visible keep the
// recap out of the text the chat draws.
package turnsummary

import (
	"regexp"
	"strings"
)

// MaxChars caps a summary, ellipsis included: room for a short recap, while
// a list row clips it at its own width.
const MaxChars = 600

const (
	openTag  = "<recap>"
	closeTag = "</recap>"
)

// Text is the summary of an agent's final message: its recap when it has one,
// else its opening prose. Plain text on one line; "" for a message with no prose.
func Text(message string) string {
	if _, recap, ok := Split(message); ok {
		return plain(strings.Split(recap, "\n"))
	}
	var prose []string
	for _, b := range parse(message) {
		if b.kind != fence {
			prose = append(prose, b.lines...)
		}
	}
	return plain(prose)
}

// Split takes the recap off an agent's message: a <recap> line outside code,
// through </recap>, keeping whatever follows (a tool's Sources list, cards).
// One left open runs to the next fence or the end, and counts only with ui
// fences after it. It returns the message without it and the recap.
func Split(message string) (rest, recap string, ok bool) {
	start := recapLine(message)
	if start < 0 {
		return message, "", false
	}
	body := message[start+strings.Index(message[start:], openTag)+len(openTag):]
	after := ""
	if loc := closeLine.FindStringIndex(body); loc != nil {
		body, after = body[:loc[1]-len(closeTag)], body[loc[1]:]
	} else {
		if loc := fenceLine.FindStringIndex(body); loc != nil {
			body, after = body[:loc[0]], body[loc[0]:]
		}
		for _, b := range parse(after) {
			if b.kind != fence || b.info != "ui" {
				return message, "", false
			}
		}
	}
	rest = strings.TrimRight(message[:start], " \t\n")
	if after = strings.TrimSpace(after); after != "" {
		rest = strings.TrimLeft(rest+"\n\n"+after, "\n")
	}
	return rest, strings.TrimSpace(body), true
}

// Strip is message without its recap, for the chat to draw.
func Strip(message string) string {
	rest, _, _ := Split(message)
	return rest
}

// Visible is a streaming turn's text as the chat draws it: mid-turn it stops
// before a recap line, or a last line that may become one, until the turn ends
// and Strip decides. It only grows within a turn, as deltas need.
func Visible(text string, done bool) string {
	if done {
		return Strip(text)
	}
	if start := recapLine(text); start >= 0 {
		return text[:start]
	}
	return text
}

// recapLine finds where the first <recap> line outside a code fence starts,
// or the unfinished last line when it is still a prefix of one; -1 for none.
func recapLine(text string) int {
	closing := ""
	for at := 0; at < len(text); {
		line := text[at:]
		next := len(text)
		if i := strings.IndexByte(line, '\n'); i >= 0 {
			line, next = line[:i], at+i+1
		}
		trimmed := strings.TrimLeft(line, " ")
		switch {
		case closing != "":
			if t := strings.TrimSpace(line); strings.HasPrefix(t, closing) && strings.Trim(t, closing[:1]) == "" {
				closing = ""
			}
		case fenceOpen.MatchString(line):
			closing = fenceOpen.FindStringSubmatch(line)[1]
		case len(line)-len(trimmed) <= 3 && strings.HasPrefix(trimmed, openTag):
			return at
		case next == len(text) && !strings.HasSuffix(text, "\n") && trimmed != "" &&
			len(line)-len(trimmed) <= 3 && strings.HasPrefix(openTag, trimmed):
			return at
		}
		at = next
	}
	return -1
}

type blockKind int

const (
	paragraph blockKind = iota
	fence
)

// block is a run of prose lines, or one code fence and its info string.
type block struct {
	kind  blockKind
	info  string
	lines []string
}

var (
	fenceOpen = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})\\s*(\\S*)")
	fenceLine = regexp.MustCompile("(?m)^ {0,3}(`{3,}|~{3,})")
	// closeLine is a </recap> that ends its line, so one quoted in inline
	// code (`</recap>`) mid-recap doesn't close it.
	closeLine = regexp.MustCompile(`(?m)</recap>[ \t]*$`)
)

// parse splits markdown into prose and code fences.
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
		if open == nil {
			blocks = append(blocks, block{kind: paragraph})
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
