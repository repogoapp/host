package capture

import "testing"

func TestInputValidKeepsTouchesOnTheScreen(t *testing.T) {
	for _, c := range []struct {
		in   Input
		want bool
	}{
		{Input{Touch: "down", X: 0.5, Y: 1}, true},
		{Input{Touch: "move", X: 1.2, Y: 0.5}, false},
		{Input{Touch: "tap", X: 0.5, Y: 0.5}, false},
		{Input{Button: "home"}, true},
		{Input{Pointer: &Pointer{X: 0.5, Y: 1}}, true},
		{Input{Pointer: &Pointer{X: 1.5, Y: 0}}, false},
		{Input{Scroll: &Scroll{DY: 2}}, true},
		{Input{Key: &Key{Keysym: 0xff0d, Down: true}}, true},
		{Input{}, false},
	} {
		if got := c.in.Valid(); got != c.want {
			t.Errorf("%+v.Valid() = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestKeysymCoversLatinControlAndUnicode(t *testing.T) {
	for r, want := range map[rune]uint32{'a': 0x61, 'é': 0xe9, '\n': 0xff0d, '\b': 0xff08, '€': 0x010020ac} {
		if got := Keysym(r); got != want {
			t.Errorf("Keysym(%q) = %#x, want %#x", r, got, want)
		}
	}
}

func TestKeyForTypesAUSKeyboard(t *testing.T) {
	for _, c := range []struct {
		r     rune
		usage int
		shift bool
	}{{'a', 0x04, false}, {'Z', 0x1D, true}, {'1', 0x1E, false}, {'0', 0x27, false}, {'!', 0x1E, true}, {' ', 0x2C, false}, {'?', 0x38, true}} {
		usage, shift, ok := keyFor(c.r)
		if !ok || usage != c.usage || shift != c.shift {
			t.Errorf("keyFor(%q) = %#x %v %v, want %#x %v", c.r, usage, shift, ok, c.usage, c.shift)
		}
	}
	if _, _, ok := keyFor('é'); ok {
		t.Error("é has a key on a US keyboard")
	}
}
