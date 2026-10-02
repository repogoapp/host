package session

import (
	"path/filepath"
	"testing"
)

func TestIsExcluded(t *testing.T) {
	cases := map[string]bool{
		"":                                      false,
		"/Users/me/Desktop/apps/repogo":         false,
		"/tmp":                                  true,
		"/tmp/ra-probe-fntF":                    true,
		"/private/tmp/ra-probe-fntF":            true,
		"/private/var/folders/x/T/codex-frames": true,
		"/tmpfoo/project":                       false,
		"/Users/me/tmp/project":                 false,
		filepath.Join(t.TempDir(), "nested"):    true,
	}
	for cwd, want := range cases {
		if got := isExcluded(cwd); got != want {
			t.Errorf("isExcluded(%q) = %v, want %v", cwd, got, want)
		}
	}
}
