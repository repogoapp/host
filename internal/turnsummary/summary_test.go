package turnsummary

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTextTakesTheRecapElseTheOpening(t *testing.T) {
	cases := []struct{ name, message, want string }{
		{"recap last", "Built it.\n\n<recap>\nThe parser `turnsummary` **landed**.\nTests pass.\n</recap>", "The parser turnsummary landed. Tests pass."},
		{"recap on one line", "Built it.\n<recap>Renamed the column.</recap>\n", "Renamed the column."},
		{"recap then ui fence", "Done.\n\n<recap>Renamed the column.</recap>\n\n```ui\n<List>\n  <Item label=\"<recap>\"/>\n</List>\n```\n", "Renamed the column."},
		{"recap left open", "Done.\n\n<recap>Renamed it.\n\n```ui\n<Text>hi</Text>\n```", "Renamed it."},
		{"recap then prose", "<recap>Early.</recap>\n\nThen the answer.", "<recap>Early.</recap> Then the answer."},
		{"recap in code", "Look:\n\n```\n<recap>in code</recap>\n```", "Look:"},
		{"blockquote is prose", "Hi.\n\n> outer\n> > inner", "Hi. outer inner"},
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

func TestStripKeepsCardsAndUnrecappedText(t *testing.T) {
	cases := []struct{ name, message, want string }{
		{"recap last", "Built it.\n\n<recap>\nDone.\n</recap>\n", "Built it."},
		{"cards kept", "Built it.\n\n<recap>Done.</recap>\n\n```ui\n<Text>hi</Text>\n```\n", "Built it.\n\n```ui\n<Text>hi</Text>\n```"},
		{"only a recap", "<recap>Done.</recap>", ""},
		{"prose after", "<recap>Early.</recap>\n\nMore.", "<recap>Early.</recap>\n\nMore."},
		{"in code", "```md\n<recap>x</recap>\n```", "```md\n<recap>x</recap>\n```"},
		{"none", "Plain.", "Plain."},
	}
	for _, c := range cases {
		if got := Strip(c.message); got != c.want {
			t.Errorf("%s: Strip = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestVisibleHoldsTheRecapBackAndOnlyGrows(t *testing.T) {
	message := "Built it.\n\nMore detail.\n\n<recap>\nDone.\n</recap>\n"
	prev := ""
	for i := 1; i <= len(message); i++ {
		got := Visible(message[:i], false)
		if !strings.HasPrefix(got, prev) {
			t.Fatalf("at %d: %q does not extend %q", i, got, prev)
		}
		if strings.Contains(got, "<") {
			t.Fatalf("at %d: shows part of the tag: %q", i, got)
		}
		prev = got
	}
	if prev != "Built it.\n\nMore detail.\n\n" {
		t.Fatalf("mid-turn = %q", prev)
	}
	if got := Visible(message, true); got != "Built it.\n\nMore detail." {
		t.Fatalf("done = %q", got)
	}
	// A line that only looked like a tag is shown once it is not one.
	if got := Visible("A\n<re", false); got != "A\n" {
		t.Fatalf("partial = %q", got)
	}
	if got := Visible("A\n<rest", false); got != "A\n<rest" {
		t.Fatalf("not a tag = %q", got)
	}
	// A recap followed by prose was not the recap: shown when the turn ends.
	if got := Visible("<recap>x</recap>\n\nMore.", true); got != "<recap>x</recap>\n\nMore." {
		t.Fatalf("not last = %q", got)
	}
}

func TestTextCapsByRunes(t *testing.T) {
	got := Text("<recap>" + strings.Repeat("é🙂", MaxChars))
	if n := utf8.RuneCountInString(got); n != MaxChars || !strings.HasSuffix(got, "…") || !utf8.ValidString(got) {
		t.Fatalf("capped to %d runes, valid %v: …%q", n, utf8.ValidString(got), got[len(got)-12:])
	}
	if got := Text("short"); got != "short" {
		t.Fatalf("short = %q", got)
	}
}
