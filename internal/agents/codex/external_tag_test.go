package codex

import "testing"

func TestIsCodexExternalTag(t *testing.T) {
	cases := map[string]bool{
		`<external_codex_apps_open_page>{"page_id":null}</external_codex_apps_open_page>`: true,
		"  <external_foo>\nx\n</external_foo>\n":                                          true,
		"<external_foo>x</external_bar>":                                                  false,
		"Look at <external_foo>x</external_foo>":                                          false,
		"<external_foo>x</external_foo> and then":                                         false,
		"hi": false,
	}
	for in, want := range cases {
		if got := isSyntheticCodexPrompt(in); got != want {
			t.Errorf("isSyntheticCodexPrompt(%q) = %v, want %v", in, got, want)
		}
	}
}
