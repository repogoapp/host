package turnsummary

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTextTakesTheRecapElseTheOpening(t *testing.T) {
	cases := []struct{ name, message, want string }{
		{"recap last", "Built it.\n\n> **Recap:** the parser `turnsummary` landed.\n> Tests pass.", "Recap: the parser turnsummary landed. Tests pass."},
		{"recap then ui fence", "Done.\n\n> Renamed the column.\n\n```ui\n<List>\n  <Item label=\"> not a quote\"/>\n</List>\n```\n", "Renamed the column."},
		{"recap then prose", "> A quote first.\n\nThen the answer.", "A quote first. Then the answer."},
		{"recap then another fence", "Intro.\n\n> Recap.\n\n```go\nx := 1\n```", "Intro. Recap."},
		{"quote in a list", "Steps:\n- > quoted in a list\n- two", "Steps: quoted in a list two"},
		{"quote in code", "Look:\n\n```\n> in code\n```", "Look:"},
		{"nested quote", "Hi.\n\n> outer\n> > inner", "outer inner"},
		{"links and images", "See [the plan](https://x.y/p) and ![shot](a.png).", "See the plan and shot."},
		{"headings and lists", "## Title\n\n1. first\n* second", "Title first second"},
		{"only a ui fence", "```ui\n<Text>hi</Text>\n```", ""},
		{"empty", "  \n", ""},
		{"unclosed fence", "Text.\n\n```ui\n<Text>", "Text."},
	}
	for _, c := range cases {
		if got := Text(c.message); got != c.want {
			t.Errorf("%s: Text = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTextCapsByRunes(t *testing.T) {
	got := Text("> " + strings.Repeat("é🙂", MaxChars))
	if n := utf8.RuneCountInString(got); n != MaxChars || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Fatalf("capped to %d runes, valid %v: …%q", n, utf8.ValidString(got), got[len(got)-12:])
	}
	if got := Text("short"); got != "short" {
		t.Fatalf("short = %q", got)
	}
}
