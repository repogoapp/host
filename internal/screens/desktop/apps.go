package desktop

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Open opens the app, or brings it forward with its windows, as a click in
// the Dock does.
func Open(ctx context.Context, bundle string) error {
	if bundle == "" || strings.ContainsAny(bundle, " /") {
		return errors.New("No app with that bundle ID.")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "open", "-b", bundle).CombinedOutput()
	if err != nil {
		return errors.New(strings.TrimSpace(string(out)))
	}
	return nil
}

// Raise brings one window to the front of its app when the host has
// Accessibility, before the app is opened; without it, opening the app
// shows whichever of its windows macOS picks.
func Raise(window uint32) { raise(pidOf(window), window) }

// Quit quits every running copy of the app, as its Quit menu item does;
// one with unsaved work asks on the Mac first.
func Quit(bundle string) { quit(bundle) }

// Icon is the app's icon as the Dock draws it, a PNG px wide; nil for an
// app that isn't installed. Drawn once per app and size.
func Icon(bundle string, px int) []byte {
	key := iconKey{bundle, px}
	iconsMu.Lock()
	png, ok := icons[key]
	iconsMu.Unlock()
	if ok {
		return png
	}
	png = icon(bundle, px)
	iconsMu.Lock()
	icons[key] = png
	iconsMu.Unlock()
	return png
}

type iconKey struct {
	bundle string
	px     int
}

var (
	iconsMu sync.Mutex
	icons   = map[iconKey][]byte{}
)
