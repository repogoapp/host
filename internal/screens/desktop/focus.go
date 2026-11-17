package desktop

import (
	"context"
	"errors"
)

// Focus is the front app's focused window, as its title bar shows it.
type Focus struct {
	// Window and Title are 0 and empty when the host can't see the window
	// (no Accessibility); its buttons then work as keys on the focused one.
	Window     uint32 `json:"window,omitempty"`
	Title      string `json:"title,omitempty"`
	FullScreen bool   `json:"full_screen,omitempty"`
	// Buttons are the title bar's buttons that work now: close, minimize
	// and full_screen. In full screen only full_screen does. There's no
	// zoom: the green button's zoom is full screen on today's macOS, and
	// the old zoom is Fill, a tile.
	Buttons []string `json:"buttons"`
	// Tiles says whether the window takes Move & Resize: false without
	// Accessibility, where the Window menu can't be pressed.
	Tiles bool `json:"tiles,omitempty"`
}

// The title bar's buttons.
const (
	ButtonClose      = "close"
	ButtonMinimize   = "minimize"
	ButtonFullScreen = "full_screen"
)

// keyButtons are the buttons that work without Accessibility, as their
// shortcuts on the focused window: ⌘W, ⌘M and ⌃⌘F.
var keyButtons = []string{ButtonClose, ButtonMinimize, ButtonFullScreen}

// tiles are the Window menu's Move & Resize items (macOS 15 and later) by
// the names the phone sends, as their identifiers: AppKit's selectors,
// the same in every language.
var tiles = map[string]string{
	"left": "_zoomLeft:", "right": "_zoomRight:", "top": "_zoomTop:", "bottom": "_zoomBottom:",
	"top_left": "_zoomTopLeft:", "top_right": "_zoomTopRight:",
	"bottom_left": "_zoomBottomLeft:", "bottom_right": "_zoomBottomRight:",
	"fill": "_zoomFill:", "center": "_zoomCenter:", "previous": "_zoomUntile:",
	// Full Screen Tile: full screen, sharing it with another window.
	"full_screen_left": "_tileLeft:", "full_screen_right": "_tileRight:",
}

// readFocus reads pid's focused window. Without Accessibility it can't
// see it, and offers the buttons that work as keys.
func readFocus(pid int) *Focus {
	if !trusted() {
		return &Focus{Buttons: keyButtons}
	}
	f := focusOf(pid)
	if f != nil {
		f.Tiles = !f.FullScreen
	}
	return f
}

// ErrNeedsKeys answers a press the host can't make through Accessibility:
// the caller sends the button's shortcut to the focused window instead.
var ErrNeedsKeys = errors.New("press it with its shortcut")

// Press presses a button on the front app's focused window through
// Accessibility. Full screen waits for its slide to finish, and close on a
// full-screen window leaves full screen first, as the Mac's own close
// does. Without Accessibility it returns ErrNeedsKeys.
func Press(ctx context.Context, button string) error {
	if !trusted() {
		return ErrNeedsKeys
	}
	pid, err := frontPID(ctx)
	if err != nil {
		return err
	}
	return press(pid, button)
}

// Tile moves the front app's focused window as its Window ▸ Move & Resize
// item does: a half, a quarter, fill, center, back, or a full-screen tile.
func Tile(ctx context.Context, tile string) error {
	identifier, ok := tiles[tile]
	if !ok {
		return errors.New("unknown tile " + tile)
	}
	if !trusted() {
		return errors.New("Moving windows needs Accessibility for RepoGo on the Mac.")
	}
	pid, err := frontPID(ctx)
	if err != nil {
		return err
	}
	return pressMenu(pid, identifier)
}

func frontPID(ctx context.Context) (int, error) {
	open, front, err := openApps(ctx)
	if err != nil {
		return 0, err
	}
	a, ok := frontApp(open, front)
	if !ok {
		return 0, errors.New("No app is in front.")
	}
	return a.pid, nil
}
