package screens

import (
	"slices"
	"testing"
)

func TestAndroidText(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string
	}{
		{"hi there", []string{"input text 'hi%sthere'"}},
		{"it's", []string{`input text 'it'\''s'`}},
		{"a%sb", []string{"input text 'a%'", "input text 'sb'"}},
		{"%s", []string{"input text '%'", "input text 's'"}},
	} {
		if got := androidText(c.text); !slices.Equal(got, c.want) {
			t.Errorf("androidText(%q) = %q, want %q", c.text, got, c.want)
		}
	}
	if got := androidKey(0xff08); got != "input keyevent KEYCODE_DEL" {
		t.Errorf("backspace = %q", got)
	}
	if got := androidKey(0xffe1); got != "" {
		t.Errorf("shift = %q, want nothing", got)
	}
}
