//go:build !(windows && cgo)

package desktop

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// The Mac's own tools for the desktop: Launch Services' open apps, the
// Dock's pinned apps and `open`. Windows has its own (desktop_windows.go).

// homeApp is first in the Dock, always: Finder.
var homeApp = App{Bundle: finder, Name: "Finder"}

// lockedMessage refuses a move while the session is locked.
const lockedMessage = "The Mac is locked. Sign in to it first."

// dockApps reads the Dock's pinned apps from its preferences, in order.
// `plutil -convert json` refuses this plist (it holds <data> and <date>),
// so it's read as XML.
func dockApps(ctx context.Context) ([]App, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "defaults", "export", "com.apple.dock", "-").Output()
	if err != nil {
		return nil, err
	}
	return parseDock(raw)
}

// openApps lists the open apps the Dock shows (Launch Services'
// "Foreground" type), windows or not, and which is in front, from
// `lsappinfo`: no permission, and no run loop, which NSWorkspace's
// running apps need to stay current in a process like the host.
func openApps(ctx context.Context) ([]openApp, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	list, err := exec.CommandContext(ctx, "lsappinfo", "list").Output()
	if err != nil {
		return nil, "", err
	}
	front, _ := exec.CommandContext(ctx, "lsappinfo", "front").Output()
	return parseOpenApps(list), strings.TrimSpace(string(front)), nil
}

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
