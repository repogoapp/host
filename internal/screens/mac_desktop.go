package screens

import (
	"context"
	"errors"
	"math"
	"runtime"
	"strconv"
	"time"

	"github.com/repogoapp/host/internal/screens/capture"
	"github.com/repogoapp/host/internal/screens/desktop"
)

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
// each change the Mac reports, after each action, and every desktopEvery.
func (c *controller) watchDesktop(ctx context.Context) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		return
	}
	changes, stop := desktop.Changes()
	defer stop()
	ticker := time.NewTicker(desktopEvery)
	defer ticker.Stop()
	for {
		if d, err := desktop.Read(ctx); err == nil {
			c.publishDesktop(viewOf(d))
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

// desktopAction runs an action on the Mac's desktop, which needs its live
// view: moves between spaces, and title-bar buttons without Accessibility,
// go through it as keys, on the same Screen Sharing path as typing.
func (m *Manager) desktopAction(ctx context.Context, id string, act func(*controller, desktop.Desktop) error) error {
	c, err := m.act(ctx, id, ControlDesktop)
	if err != nil {
		return err
	}
	if !c.streaming() {
		return ErrNotStreaming
	}
	defer c.refreshDesktopSettled(ctx)
	d, err := desktop.Read(ctx)
	if err != nil {
		return err
	}
	if d.Locked {
		return desktop.ErrLocked
	}
	return act(c, d)
}

// DesktopSpace moves to the space with that id.
func (m *Manager) DesktopSpace(ctx context.Context, id, space string) error {
	target, err := strconv.ParseUint(space, 10, 64)
	if err != nil {
		return errInvalid.Errorf("no space %q", space)
	}
	return m.desktopAction(ctx, id, func(c *controller, d desktop.Desktop) error { return c.moveTo(d, target, c.forward) })
}

// DesktopOpenApp opens or brings forward an app.
func (m *Manager) DesktopOpenApp(ctx context.Context, id, bundle string) error {
	return m.desktopAction(ctx, id, func(*controller, desktop.Desktop) error { return desktop.Open(ctx, bundle) })
}

// DesktopQuitApp quits an app.
func (m *Manager) DesktopQuitApp(ctx context.Context, id, bundle string) error {
	return m.desktopAction(ctx, id, func(*controller, desktop.Desktop) error {
		desktop.Quit(bundle)
		return nil
	})
}

// DesktopWindow shows a window: its space, raised, its app forward.
func (m *Manager) DesktopWindow(ctx context.Context, id string, window int) error {
	if window <= 0 || window > math.MaxUint32 {
		return errInvalid.Errorf("no window %d", window)
	}
	return m.desktopAction(ctx, id, func(c *controller, d desktop.Desktop) error {
		return c.showWindow(ctx, d, uint32(window), c.forward)
	})
}

// DesktopButton presses the focused window's title-bar button.
func (m *Manager) DesktopButton(ctx context.Context, id string, button WindowButton) error {
	keys, ok := buttonKeys[string(button)]
	if !ok {
		return errInvalid.Errorf("unknown button %q", button)
	}
	return m.desktopAction(ctx, id, func(c *controller, _ desktop.Desktop) error {
		err := desktop.Press(ctx, string(button))
		if errors.Is(err, desktop.ErrNeedsKeys) {
			return chord(c.forward, keys...)
		}
		return err
	})
}

// DesktopTile moves and resizes the focused window, as Window ▸ Move & Resize does.
func (m *Manager) DesktopTile(ctx context.Context, id string, tile Tile) error {
	return m.desktopAction(ctx, id, func(*controller, desktop.Desktop) error { return desktop.Tile(ctx, string(tile)) })
}

// Launchpad is every installed app, by name.
func (m *Manager) Launchpad(ctx context.Context, id string) ([]App, error) {
	if _, err := m.act(ctx, id, ControlDesktop); err != nil {
		return nil, err
	}
	apps := []App{}
	for _, a := range desktop.Installed() {
		apps = append(apps, App{Bundle: a.Bundle, Name: a.Name})
	}
	return apps, nil
}

// Icons are the apps' icons; an app that isn't installed is left out, and
// the phone keeps its placeholder.
func (m *Manager) Icons(ctx context.Context, id string, bundles []string) ([]Icon, error) {
	if _, err := m.act(ctx, id, ControlDesktop); err != nil {
		return nil, err
	}
	icons := []Icon{}
	for _, b := range bundles[:min(len(bundles), iconsAtOnce)] {
		if png := desktop.Icon(b, iconSize); png != nil {
			icons = append(icons, Icon{Bundle: b, Image: png})
		}
	}
	return icons, nil
}

// swipe posts one step of a live space swipe. Without Accessibility the
// steps do nothing, and an end that would have finished the move jumps a
// space with Control-arrow instead. Progress is in spaces; an end's
// direction is -1, 1, or 0 to slide back, signed as progress is.
func (c *controller) swipe(ctx context.Context, in SpaceSwipeInput, forward func(capture.Input) error) error {
	clamp := func(f float64) float64 {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0
		}
		return max(-8, min(8, f))
	}
	var phase desktop.SwipePhase
	velocity := 0.0
	switch in.Phase {
	case SwipeBegan:
		phase = desktop.SwipeBegan
	case SwipeChanged:
		phase = desktop.SwipeChanged
	case SwipeEnded:
		phase = desktop.SwipeEnded
		if in.Direction != nil {
			velocity = float64(max(-1, min(1, *in.Direction))) * 1000
		}
	case SwipeCancelled:
		phase = desktop.SwipeCancelled
	default:
		return errInvalid.Errorf("unknown swipe phase %q", in.Phase)
	}
	err := desktop.Swipe(phase, clamp(in.Progress), velocity)
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
	// Positive is toward the space on the right.
	next := d.Current - 1
	if velocity > 0 {
		next = d.Current + 1
	}
	if next < 0 || next >= len(d.Spaces) {
		return nil
	}
	return c.moveTo(d, d.Spaces[next].ID, forward)
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
