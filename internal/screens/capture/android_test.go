//go:build darwin && cgo

package capture

import "testing"

func TestPanelUndoesTheTurn(t *testing.T) {
	for _, c := range []struct {
		shape  frameShape
		fx, fy float64
		x, y   int
	}{
		{frameShape{1080, 2400, 0}, 0, 0, 0, 0},
		{frameShape{1080, 2400, 2}, 0, 0, 1079, 2399},
		// Turned a quarter: the frame's left-middle (the q key) is the panel's lower middle.
		{frameShape{2400, 1080, 3}, 0, 0.5, 540, 2399},
		{frameShape{2400, 1080, 1}, 0, 0.5, 540, 0},
	} {
		if x, y := c.shape.panel(c.fx, c.fy); x != c.x || y != c.y {
			t.Errorf("%+v (%v, %v) = (%d, %d), want (%d, %d)", c.shape, c.fx, c.fy, x, y, c.x, c.y)
		}
	}
}
