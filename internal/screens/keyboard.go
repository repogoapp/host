package screens

import (
	"context"
	"math"
	"strings"
	"time"
)

// Sending the on-screen keyboard away, for agents: Android's Escape key does
// it (Back can close the screen with it); iOS has no key for it, so it goes
// the way a person sends it, a short drag on the screen above it, which a
// scrolling list takes as the touch that dismisses it.

// HideKeyboard hides the on-screen keyboard, if it's up.
func (m *Manager) HideKeyboard(ctx context.Context, id string) error {
	c, unlock, err := m.lockInput(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	read, err := c.readElements(ctx)
	if err != nil || !read.Keyboard {
		return err
	}
	if c.d.Platform == PlatformAndroid {
		defer c.screenChanged()
		_, err := c.shell(ctx, "input keyevent KEYCODE_ESCAPE")
		return err
	}
	// Sideways first: it scrolls nothing, so a list takes it as the touch that
	// dismisses; then up, for a list that dismisses only on a scroll.
	for _, along := range []Point{{X: -0.03}, {Y: -0.03}} {
		from, ok := dragStart(read)
		if !ok {
			return errInvalid.Errorf("every point above the keyboard is on a control, so a drag would press one: " +
				keyboardAdvice)
		}
		to := Point{X: from.X + along.X*read.Width, Y: from.Y + along.Y*read.Height}
		if err := c.gesture(ctx, []Point{from, to}, 250); err != nil {
			return err
		}
		time.Sleep(keyboardSlide)
		if read, err = c.readElements(ctx); err != nil || !read.Keyboard {
			return err
		}
	}
	return errInvalid.Errorf("the keyboard stayed up through a sideways and an upward drag: " + keyboardAdvice)
}

// keyboardAdvice is what works when a drag doesn't.
const keyboardAdvice = "tap the app's own Done or close button, or press return where submitting is what you want"

// keyboardSlide is how long the keyboard takes to slide away.
const keyboardSlide = 400 * time.Millisecond

// controlClearance keeps a drag this far from any control: a touch's area
// runs past its frame, and a drag's first points still count as a tap.
const controlClearance = 8

// dragStart is the point nearest the middle of the screen above the
// keyboard that's clear of every control, away from the screen's edges,
// where the system's own gestures start.
func dragStart(read Elements) (Point, bool) {
	top, bottom := read.Height*0.1, read.keyboardTop-controlClearance
	left, right := read.Width*0.1, read.Width*0.9
	middle := Point{X: read.Width / 2, Y: (top + bottom) / 2}
	step := min(read.Width, read.Height) / 40
	best, found, bestDistance := Point{}, false, math.Inf(1)
	for y := top; y <= bottom; y += step {
		for x := left; x <= right; x += step {
			p := Point{X: x, Y: y}
			if onControl(read, p) {
				continue
			}
			if d := math.Hypot(p.X-middle.X, p.Y-middle.Y); d < bestDistance {
				best, found, bestDistance = p, true, d
			}
		}
	}
	return best, found
}

// onControl is whether p is on, or near, something a touch would press.
func onControl(read Elements, p Point) bool {
	screen := read.Width * read.Height
	for _, e := range read.Elements {
		// Text and pictures do nothing when touched; what covers most of the
		// screen is a container standing for its children.
		if isStatic(e.Role) || e.W*e.H > screen/2 {
			continue
		}
		if p.X >= e.X-controlClearance && p.X <= e.X+e.W+controlClearance &&
			p.Y >= e.Y-controlClearance && p.Y <= e.Y+e.H+controlClearance {
			return true
		}
	}
	return false
}

// isStatic is whether a role is content a touch does nothing to.
func isStatic(role string) bool {
	for _, static := range []string{"StaticText", "Image", "Heading", "TextView", "ImageView"} {
		if strings.EqualFold(role, static) {
			return true
		}
	}
	return false
}
