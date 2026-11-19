package desktop

import "errors"

// SwipePhase is a space swipe's step, as the Dock reads it.
type SwipePhase int

const (
	SwipeBegan     SwipePhase = 1
	SwipeChanged   SwipePhase = 2
	SwipeEnded     SwipePhase = 4
	SwipeCancelled SwipePhase = 8
)

// ErrNoSwipe answers a swipe the host can't post (no Accessibility): the
// caller moves a whole space with Control-arrow once it ends instead.
var ErrNoSwipe = errors.New("swipe with Control-arrow")

// Swipe posts one step of the three-finger swipe between spaces, which the
// Mac follows live. Progress is in spaces, positive toward the space on the
// right (seen on the phone 11-19, when the Spaces pager first moved the Mac). An end needs a velocity in the
// swipe's direction to finish the move; below that it, and a cancel, slide
// back. The events are undocumented: a macOS update can stop them working.
func Swipe(phase SwipePhase, progress, velocity float64) error {
	if !trusted() {
		return ErrNoSwipe
	}
	swipe(int(phase), progress, velocity)
	return nil
}
