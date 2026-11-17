// Package desktop reads and drives this Mac's desktop as the Dock, Mission
// Control and a window's title bar give it: the spaces in swipe order, the
// open and pinned apps with their windows, the front app's focused window
// and its buttons, every installed app for a Launchpad, and app icons. The
// Mac screen's live view sends it to the phone.
//
// Each app is listed once, in Apps, in the Dock's order; the front app and
// a full-screen space name apps by bundle ID. Spaces come from SkyLight's
// private CGSCopyManagedDisplaySpaces, which a macOS update can change:
// Read then returns no spaces and the phone hides its dots.
package desktop

import (
	"context"
	"errors"
	"slices"
	"strings"
)

// Desktop is the Mac's desktop as the phone draws it.
type Desktop struct {
	// Locked is set while the session is locked, and nothing else is: no
	// move works until the Mac is signed in to.
	Locked bool `json:"locked,omitempty"`
	// Spaces are the main display's, in swipe order; Current is the one showing.
	Spaces  []Space `json:"spaces,omitempty"`
	Current int     `json:"current"`
	// Apps are the Dock's, once each, in its order: Finder, the pinned
	// apps, then open ones that aren't pinned.
	Apps []App `json:"apps,omitempty"`
	// Front is the front app's bundle ID; Focus its focused window.
	Front string `json:"front,omitempty"`
	Focus *Focus `json:"focus,omitempty"`
}

// Space is one desktop or full-screen app.
type Space struct {
	ID uint64 `json:"id"`
	// Apps are a full-screen space's apps by bundle ID, two when side by
	// side; empty for a desktop.
	Apps []string `json:"apps,omitempty"`
}

// App is one app: open, pinned to the Dock, or both; for the Launchpad,
// installed.
type App struct {
	Bundle  string   `json:"bundle"`
	Name    string   `json:"name"`
	Running bool     `json:"running,omitempty"`
	Windows []Window `json:"windows,omitempty"`
}

// Window is one of an app's windows. Title is empty without the Screen
// Recording permission; Space is 0 when SkyLight doesn't say.
type Window struct {
	ID    uint32 `json:"id"`
	Title string `json:"title,omitempty"`
	Space uint64 `json:"space,omitempty"`
}

// ErrLocked refuses a move while the session is locked, where it would
// silently do nothing.
var ErrLocked = errors.New("The Mac is locked. Sign in to it first.")

// Read reads the desktop now.
func Read(ctx context.Context) (Desktop, error) {
	if locked() {
		return Desktop{Locked: true}, nil
	}
	pinned, err := dockApps(ctx)
	if err != nil {
		return Desktop{}, err
	}
	open, front, err := openApps(ctx)
	if err != nil {
		return Desktop{}, err
	}
	spaces, current, windows := read()
	d := Desktop{Spaces: spaces, Current: current, Apps: merge(pinned, open, windows)}
	if a, ok := frontApp(open, front); ok {
		d.Front = a.Bundle
		d.Focus = readFocus(a.pid)
	}
	return d, nil
}

// frontApp is the open app Launch Services names as in front.
func frontApp(open []openApp, front string) (openApp, bool) {
	for _, a := range open {
		if a.asn == front {
			return a, true
		}
	}
	return openApp{}, false
}

// openApp is an app Launch Services lists as open in the Dock, with its
// process and Launch Services' name for it (ASN:0x0-0x11011:).
type openApp struct {
	App
	pid int
	asn string
}

// rawWindow is an app window as the window list and SkyLight report it.
type rawWindow struct {
	ID     uint32
	Bundle string
	App    string
	Title  string
	Space  uint64
}

// merge lists each app once, in the Dock's order (Finder, the pinned apps,
// then open apps that aren't pinned), each with its windows.
func merge(pinned []App, open []openApp, windows []rawWindow) []App {
	apps := append([]App{{Bundle: finder, Name: "Finder"}}, pinned...)
	index := map[string]int{}
	add := func(a App) int {
		if i, ok := index[a.Bundle]; ok {
			return i
		}
		index[a.Bundle] = len(apps)
		apps = append(apps, a)
		return len(apps) - 1
	}
	for i, a := range apps {
		index[a.Bundle] = i
	}
	for _, a := range open {
		apps[add(a.App)].Running = true
	}
	// With titles readable (Screen Recording), an untitled window is a
	// helper (Chrome's tab strip overlays); without them, all are listed.
	titled := slices.ContainsFunc(windows, func(w rawWindow) bool { return w.Title != "" })
	for _, w := range windows {
		if titled && w.Title == "" {
			continue
		}
		i := add(App{Bundle: w.Bundle, Name: w.App})
		apps[i].Running = true
		apps[i].Windows = append(apps[i].Windows, Window{ID: w.ID, Title: w.Title, Space: w.Space})
	}
	for i := range apps {
		slices.SortStableFunc(apps[i].Windows, func(a, b Window) int { return strings.Compare(a.Title, b.Title) })
	}
	return apps
}

const finder = "com.apple.finder"
