package chat

import (
	"strings"
	"testing"
)

func TestCheckedTitle(t *testing.T) {
	for answer, want := range map[string]string{
		"Make Send Button Bigger":            "Make Send Button Bigger",
		"\"Fix Login Flash\".":               "Fix Login Flash",
		"**Push Changes to GitHub**\n\nDone": "Push Changes to GitHub",
		"can you make the send button bigger its to small on the iphone please": "",
		"Is This the Right Title?": "",
		"make send button bigger":  "",
		"":                         "",
	} {
		if got := checkedTitle(answer); got != want {
			t.Errorf("checkedTitle(%q) = %q, want %q", answer, got, want)
		}
	}
}

func TestTitleRequestFencesAndClipsThePrompt(t *testing.T) {
	req := titleRequest(strings.Repeat("a", titleMessageLimit+50))
	if !strings.Contains(req, "<message>\n"+strings.Repeat("a", titleMessageLimit)+"…\n</message>") {
		t.Fatal("a long prompt is not clipped inside the message tags")
	}
}
