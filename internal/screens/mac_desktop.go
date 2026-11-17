package screens

import (
	"context"
	"encoding/base64"
	"errors"
	"math"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/repogoapp/host/internal/screens/capture"
	"github.com/repogoapp/host/internal/screens/desktop"
)

// The Mac screen's desktop actions, run by the host. Value is what each
// acts on: a space's ID, an app's bundle ID, a window's ID, a title-bar
// button (close, minimize, full_screen), a tile (left, top_right, fill),
// a swipe step (parseSwipe), or bundle IDs joined by commas for icons.
const (
	actionSpace     = "desktop_space"
	actionApp       = "desktop_app"
	actionWindow    = "desktop_window"
	actionButton    = "desktop_button"
	actionTile      = "desktop_tile"
	actionSwipe     = "desktop_swipe"
	actionQuit      = "desktop_quit"
	actionLaunchpad = "desktop_launchpad"
	actionIcons     = "desktop_icons"
)

// isDesktopAction routes an action to the host by its prefix; actDisplay
// refuses one it doesn't know.
func isDesktopAction(action string) bool { return strings.HasPrefix(action, "desktop_") }

// desktopEvery is how often the desktop is read while a phone watches, on
// top of the Mac's own change notices: windows moving between spaces and
// a Dock edit come with no notice. It's sent only when it changes.
const desktopEvery = 2 * time.Second

// iconSize is an app icon's width in pixels: 32-point icons at 3x.
const iconSize = 96

// iconsAtOnce bounds one icons request, a screenful of Launchpad.
const iconsAtOnce = 60

// X keysyms for the shortcuts the host types on the Mac.
const (
	keysymControl = 0xFFE3
	keysymCommand = 0xFFEB // Super_L, which Screen Sharing takes as ⌘
	keysymLeft    = 0xFF51
	keysymRight   = 0xFF53
)

// buttonKeys are the title-bar buttons' shortcuts, for a host without
// Accessibility: ⌘W, ⌘M and ⌃⌘F, on the focused window.
var buttonKeys = map[string][]uint32{
	desktop.ButtonClose:      {keysymCommand, 'w'},
	desktop.ButtonMinimize:   {keysymCommand, 'm'},
	desktop.ButtonFullScreen: {keysymControl, keysymCommand, 'f'},
}

// watchDesktop sends the Mac's desktop while ctx lasts: at once, then on
// each change the Mac reports, after each action, and every desktopEvery,
// with each app's icon the first time it shows.
func (c *controller) watchDesktop(ctx context.Context) {
	if runtime.GOOS != "darwin" {
		return
	}
	changes, stop := desktop.Changes()
	defer stop()
	ticker := time.NewTicker(desktopEvery)
	defer ticker.Stop()
	sent := map[string]bool{}
	var last *desktop.Desktop
	for {
		if d, err := desktop.Read(ctx); err == nil && (last == nil || !reflect.DeepEqual(d, *last)) {
			var bundles []string
			for _, a := range d.Apps {
				if !sent[a.Bundle] {
					sent[a.Bundle] = true
					bundles = append(bundles, a.Bundle)
				}
			}
			c.sendIcons(bundles)
			c.reply(Message{Desktop: &d})
			last = &d
		}
		select {
		case <-ctx.Done():
			return
		case <-changes:
			// A burst of notices (a window closing fires several) is one read.
			time.Sleep(100 * time.Millisecond)
		case <-c.refresh:
		case <-ticker.C:
		}
	}
}

// sendIcons sends the apps' icons in one message; an app that isn't
// installed is left out, and the phone keeps its placeholder.
func (c *controller) sendIcons(bundles []string) {
	icons := map[string]string{}
	for _, b := range bundles {
		if png := desktop.Icon(b, iconSize); png != nil {
			icons[b] = base64.StdEncoding.EncodeToString(png)
		}
	}
	if len(icons) > 0 {
		c.reply(Message{Icons: icons})
	}
}

// actDisplay runs a desktop action. Moves between spaces, and title-bar
// buttons without Accessibility, go through forward as keys, on the same
// Screen Sharing path as typing.
func (c *controller) actDisplay(ctx context.Context, in capture.Input, forward func(capture.Input) error) error {
	switch in.Action {
	case actionLaunchpad:
		c.reply(Message{Launchpad: desktop.Installed()})
		return nil
	case actionIcons:
		bundles := strings.Split(in.Value, ",")
		c.sendIcons(bundles[:min(len(bundles), iconsAtOnce)])
		return nil
	case actionSwipe:
		// Ahead of the desktop read: a step comes with each finger move.
		return c.swipe(ctx, in.Value, forward)
	}
	defer c.refreshDesktopSettled(ctx)
	d, err := desktop.Read(ctx)
	if err != nil {
		return err
	}
	if d.Locked {
		return desktop.ErrLocked
	}
	switch in.Action {
	case actionSpace:
		id, err := strconv.ParseUint(in.Value, 10, 64)
		if err != nil {
			return errors.New("no space " + in.Value)
		}
		return c.moveTo(d, id, forward)
	case actionApp:
		return desktop.Open(ctx, in.Value)
	case actionWindow:
		id, err := strconv.ParseUint(in.Value, 10, 32)
		if err != nil {
			return errors.New("no window " + in.Value)
		}
		return c.showWindow(ctx, d, uint32(id), forward)
	case actionButton:
		err := desktop.Press(ctx, in.Value)
		if errors.Is(err, desktop.ErrNeedsKeys) {
			keys, ok := buttonKeys[in.Value]
			if !ok {
				return errors.New("unknown button " + in.Value)
			}
			return chord(forward, keys...)
		}
		return err
	case actionTile:
		return desktop.Tile(ctx, in.Value)
	case actionQuit:
		desktop.Quit(in.Value)
		return nil
	}
	return errors.New("unknown desktop action " + in.Action)
}

// swipe posts one step of a live space swipe. Without Accessibility the
// steps do nothing, and an end that would have finished the move jumps a
// space with Control-arrow instead.
func (c *controller) swipe(ctx context.Context, value string, forward func(capture.Input) error) error {
	phase, progress, velocity, err := parseSwipe(value)
	if err != nil {
		return err
	}
	err = desktop.Swipe(phase, progress, velocity)
	if phase == desktop.SwipeEnded || phase == desktop.SwipeCancelled {
		defer c.refreshDesktopSettled(ctx)
	}
	if !errors.Is(err, desktop.ErrNoSwipe) || phase != desktop.SwipeEnded || velocity == 0 {
		return nil
	}
	d, err := desktop.Read(ctx)
	if err != nil || d.Locked {
		return err
	}
	// Negative is toward the space on the right, as the fingers move left.
	next := d.Current + 1
	if velocity > 0 {
		next = d.Current - 1
	}
	if next < 0 || next >= len(d.Spaces) {
		return nil
	}
	return c.moveTo(d, d.Spaces[next].ID, forward)
}

// parseSwipe reads a swipe step as the phone sends it: "began",
// "changed:<progress>", "ended:<progress>:<direction>" or
// "cancelled:<progress>". Progress is in spaces; an end's direction is
// -1, 1, or 0 to slide back, signed as progress is. The host turns the
// direction into the Dock's velocity.
func parseSwipe(value string) (phase desktop.SwipePhase, progress, velocity float64, err error) {
	name, rest, _ := strings.Cut(value, ":")
	numbers := strings.Split(rest, ":")
	number := func(i int) (float64, error) {
		if rest == "" || i >= len(numbers) {
			return 0, errors.New("swipe step " + value + " is missing a number")
		}
		f, err := strconv.ParseFloat(numbers[i], 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, errors.New("swipe step " + value + " has a bad number")
		}
		return max(-8, min(8, f)), nil
	}
	switch name {
	case "began":
		return desktop.SwipeBegan, 0, 0, nil
	case "changed":
		progress, err = number(0)
		return desktop.SwipeChanged, progress, 0, err
	case "ended":
		if progress, err = number(0); err != nil {
			return 0, 0, 0, err
		}
		direction, err := number(1)
		return desktop.SwipeEnded, progress, direction * 1000, err
	case "cancelled":
		progress, err = number(0)
		return desktop.SwipeCancelled, progress, 0, err
	}
	return 0, 0, 0, errors.New("unknown swipe step " + value)
}

// showWindow goes to the window's space, raises it, and opens its app.
// Going to the space first matters: an app with windows on several
// spaces otherwise comes forward on whichever macOS picks.
func (c *controller) showWindow(ctx context.Context, d desktop.Desktop, id uint32, forward func(capture.Input) error) error {
	for _, a := range d.Apps {
		for _, w := range a.Windows {
			if w.ID != id {
				continue
			}
			if w.Space != 0 {
				if err := c.moveTo(d, w.Space, forward); err != nil {
					return err
				}
			}
			desktop.Raise(w.ID)
			return desktop.Open(ctx, a.Bundle)
		}
	}
	return errors.New("That window has closed.")
}

// moveTo moves from the current space to the one with id, a space at a
// time with Control-Left or Right, and waits for the slides to finish.
func (c *controller) moveTo(d desktop.Desktop, id uint64, forward func(capture.Input) error) error {
	target := -1
	for i, s := range d.Spaces {
		if s.ID == id {
			target = i
		}
	}
	if target < 0 {
		return errors.New("That space has closed.")
	}
	steps, arrow := target-d.Current, uint32(keysymRight)
	if steps < 0 {
		steps, arrow = -steps, keysymLeft
	}
	for range steps {
		if err := chord(forward, keysymControl, arrow); err != nil {
			return err
		}
	}
	if steps > 0 {
		time.Sleep(time.Duration(300+200*steps) * time.Millisecond)
	}
	return nil
}

// chord presses keys in order and releases them in reverse, each its own
// event 20 ms apart: macOS ignores a key that only carries a modifier's flag.
func chord(forward func(capture.Input) error, keys ...uint32) error {
	press := func(k uint32, down bool) error {
		err := forward(capture.Input{Key: &capture.Key{Keysym: k, Down: down}})
		time.Sleep(20 * time.Millisecond)
		return err
	}
	for _, k := range keys {
		if err := press(k, true); err != nil {
			return err
		}
	}
	for i := len(keys) - 1; i >= 0; i-- {
		if err := press(keys[i], false); err != nil {
			return err
		}
	}
	return nil
}

// settleReads are when the desktop is read again after an action: a
// closed window leaves its app with no focused window for a moment, then
// the next window or app comes forward, with no notice the host hears.
var settleReads = []time.Duration{400 * time.Millisecond, 1200 * time.Millisecond}

// refreshDesktopSettled reads the desktop now and again as the Mac settles
// after an action.
func (c *controller) refreshDesktopSettled(ctx context.Context) {
	c.refreshDesktop()
	go func() {
		start := time.Now()
		for _, at := range settleReads {
			select {
			case <-ctx.Done():
				return
			case <-time.After(at - time.Since(start)):
				c.refreshDesktop()
			}
		}
	}()
}

func (c *controller) refreshDesktop() {
	select {
	case c.refresh <- struct{}{}:
	default:
	}
}
