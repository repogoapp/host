package desktop

import (
	"sync"
)

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
