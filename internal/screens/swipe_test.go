package screens

import (
	"testing"

	"github.com/repogoapp/host/internal/screens/desktop"
)

// The phone's swipe steps read back as the Dock's phases, with progress
// and velocity bounded; a missing or bad number is refused, not zero.
func TestParseSwipe(t *testing.T) {
	cases := []struct {
		in       string
		phase    desktop.SwipePhase
		progress float64
		velocity float64
	}{
		{"began", desktop.SwipeBegan, 0, 0},
		{"changed:-0.42", desktop.SwipeChanged, -0.42, 0},
		{"ended:-0.6:-1", desktop.SwipeEnded, -0.6, -1000},
		{"ended:0.2:0", desktop.SwipeEnded, 0.2, 0},
		{"cancelled:0.1", desktop.SwipeCancelled, 0.1, 0},
		{"changed:99", desktop.SwipeChanged, 8, 0},
	}
	for _, c := range cases {
		phase, progress, velocity, err := parseSwipe(c.in)
		if err != nil || phase != c.phase || progress != c.progress || velocity != c.velocity {
			t.Errorf("%q: %v %v %v %v", c.in, phase, progress, velocity, err)
		}
	}
	for _, bad := range []string{"fling", "changed", "changed:", "changed:NaN", "ended:-0.5", "cancelled:x"} {
		if _, _, _, err := parseSwipe(bad); err == nil {
			t.Errorf("%q: no error", bad)
		}
	}
}
