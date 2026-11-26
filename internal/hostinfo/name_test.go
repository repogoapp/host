package hostinfo

import (
	"errors"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/errkind"
)

// A rename is saved and told to every device; clearing it, or naming the
// machine its own name, goes back to the default; an overlong one is refused.
func TestRenameSavesAndTellsEveryDevice(t *testing.T) {
	saved := ""
	var told []NameChanged
	s := New(Deps{DefaultName: "Servers-Mac-Studio", SavedName: func() string { return saved },
		SaveName: func(name string) error { saved = name; return nil }, NameChanged: func(ev NameChanged) { told = append(told, ev) },
		Power: reading{}, Relay: func() bool { return false }, ReadDevice: func() Device { return studio }})

	if got, err := s.Rename("  Build   Box "); err != nil || got.Name != "Build Box" || saved != "Build Box" {
		t.Fatalf("rename: %+v, %v, saved %q", got, err, saved)
	}
	if got, _ := s.Rename(""); got.Name != "Servers-Mac-Studio" || saved != "" {
		t.Fatalf("clear: %+v, saved %q", got, saved)
	}
	s.Rename("Build Box")
	if got, _ := s.Rename("Servers-Mac-Studio"); got.Name != "Servers-Mac-Studio" || saved != "" {
		t.Fatalf("its own name: %+v, saved %q, want it cleared", got, saved)
	}
	if _, err := s.Rename(strings.Repeat("x", NameMaxRunes+1)); !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("a long name: %v, want invalid", err)
	}
	for _, bad := range []string{"Build\x1b[31mBox", "Studio\u202eoiduts", "a\u200bb"} {
		if _, err := s.Rename(bad); !errors.Is(err, errkind.ErrInvalid) {
			t.Errorf("%q: %v, want invalid", bad, err)
		}
	}
	if len(told) != 4 || told[0].Name != "Build Box" || told[1].Name != "Servers-Mac-Studio" || told[0].DefaultName != "Servers-Mac-Studio" {
		t.Fatalf("devices heard %+v", told)
	}
}
