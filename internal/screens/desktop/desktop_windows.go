//go:build windows && cgo

package desktop

/*
#cgo LDFLAGS: -ldwmapi -lshell32 -lole32 -lpropsys -lversion -luser32 -lgdi32
#include <stdlib.h>
#include "desktop_windows.h"
*/
import "C"

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"image"
	"image/png"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unsafe"
)

// On Windows the desktop is what the taskbar and Alt+Tab show: File
// Explorer first, as Finder is on a Mac, then the apps with windows, in the
// order they first opened. An app is its AppUserModelID, as the taskbar
// groups it, or its program's path when it has none. There are no spaces
// (virtual desktops have no public API), and no pinned apps yet.

// explorerID is File Explorer's AppUserModelID.
const explorerID = "Microsoft.Windows.Explorer"

var homeApp = App{Bundle: explorerID, Name: "File Explorer"}

const lockedMessage = "Windows is locked. Sign in to it first."

// maxWindows bounds one read of the windows.
const maxWindows = 256

var setup = sync.OnceFunc(func() { C.wd_init() })

// window is one app window, front to back as Windows stacks them.
type window struct {
	id                    uint32
	pid                   int
	title, app, exe       string
	canMinimize, canMaxim bool
}

// key is the app a window belongs to.
func (w window) key() string {
	if w.app != "" {
		return w.app
	}
	if strings.EqualFold(filepath.Base(w.exe), "explorer.exe") {
		return explorerID
	}
	return w.exe
}

// sameApp compares app IDs as Windows does: a program's path in any case
// (the Start menu says system32 where a window says System32).
func sameApp(a, b string) bool {
	if strings.Contains(a, `\`) {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func fromC(c *C.wd_window) window {
	return window{
		id: uint32(c.hwnd), pid: int(c.pid),
		title: C.GoString(&c.title[0]), app: C.GoString(&c.app[0]), exe: C.GoString(&c.exe[0]),
		canMinimize: c.canMinimize != 0, canMaxim: c.canMaximize != 0,
	}
}

// listWindows reads the app windows, front to back. The shell's calls
// want COM on one thread throughout.
func listWindows() []window {
	setup()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	buf := make([]C.wd_window, maxWindows)
	n := int(C.wd_windows(&buf[0], C.int(len(buf))))
	out := make([]window, 0, n)
	for i := range n {
		out = append(out, fromC(&buf[i]))
	}
	return out
}

// foreground is the front window, and whether it's an app's: false for
// the desktop or the taskbar, which belong to File Explorer.
func foreground() (window, bool, bool) {
	setup()
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var c C.wd_window
	switch C.wd_foreground(&c) {
	case 1:
		return fromC(&c), true, true
	case 2:
		return fromC(&c), false, true
	}
	return window{}, false, false
}

// Names and first-seen order, per app, kept for the host's life: the
// Dock's order holds as apps come and go.
var (
	appsMu sync.Mutex
	names  = map[string]string{}
	order  = map[string]int{}
)

func nameOf(key string) string {
	appsMu.Lock()
	name, ok := names[key]
	appsMu.Unlock()
	if ok {
		return name
	}
	if key == explorerID {
		name = homeApp.Name
	} else {
		runtime.LockOSThread()
		c := C.CString(key)
		if p := C.wd_name(c); p != nil {
			name = C.GoString(p)
			C.free(unsafe.Pointer(p))
		}
		C.free(unsafe.Pointer(c))
		runtime.UnlockOSThread()
	}
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(key), filepath.Ext(key))
	}
	appsMu.Lock()
	names[key] = name
	appsMu.Unlock()
	return name
}

func orderOf(key string) int {
	appsMu.Lock()
	defer appsMu.Unlock()
	if i, ok := order[key]; ok {
		return i
	}
	order[key] = len(order)
	return order[key]
}

// dockApps would be the taskbar's pinned apps; not read yet.
func dockApps(context.Context) ([]App, error) { return nil, nil }

// openApps lists the apps with windows, in the order they first opened,
// and the front one: File Explorer when the desktop or taskbar is in front.
func openApps(context.Context) ([]openApp, string, error) {
	var out []openApp
	seen := map[string]bool{}
	add := func(w window) {
		k := w.key()
		if k == "" || seen[k] {
			return
		}
		seen[k] = true
		out = append(out, openApp{App: App{Bundle: k, Name: nameOf(k)}, pid: w.pid, asn: k})
	}
	for _, w := range listWindows() {
		add(w)
	}
	front := ""
	if w, _, ok := foreground(); ok {
		add(w)
		front = w.key()
	}
	slices.SortStableFunc(out, func(a, b openApp) int { return cmp.Compare(orderOf(a.Bundle), orderOf(b.Bundle)) })
	return out, front, nil
}

func locked() bool  { return C.wd_locked() != 0 }
func trusted() bool { return true }

func scrollEdge() (int, [4]float64, bool) { return -1, [4]float64{}, false }

func read() ([]Space, int, []rawWindow) {
	var out []rawWindow
	for _, w := range listWindows() {
		k := w.key()
		out = append(out, rawWindow{ID: w.id, Bundle: k, App: nameOf(k), Title: w.title})
	}
	return nil, 0, out
}

// focusOf is the front window, with the buttons its title bar has; none
// when the desktop or taskbar is in front. Maximize is the full-screen
// button's place.
func focusOf(int) *Focus {
	w, isApp, ok := foreground()
	if !ok || !isApp {
		return &Focus{Buttons: []string{}}
	}
	f := &Focus{Window: w.id, Title: w.title, Buttons: []string{ButtonClose}}
	if w.canMinimize {
		f.Buttons = append(f.Buttons, ButtonMinimize)
	}
	if w.canMaxim {
		f.Buttons = append(f.Buttons, ButtonFullScreen)
	}
	return f
}

var buttonCommands = map[string]C.int{ButtonClose: 0, ButtonMinimize: 1, ButtonFullScreen: 2}

func press(_ int, button string) error {
	command, ok := buttonCommands[button]
	if !ok {
		return errors.New("unknown button " + button)
	}
	w, isApp, ok := foreground()
	if !ok || !isApp {
		return errors.New("No window is in front.")
	}
	if C.wd_button(C.uint32_t(w.id), command) != 0 {
		return errors.New("That window has closed.")
	}
	return nil
}

// untiled are windows' frames before their last tile, for "previous".
var (
	untiledMu sync.Mutex
	untiled   = map[uint32][4]int32{}
)

// pressMenu moves the front window as a Move & Resize tile, by the tile's
// Mac identifier: halves and quarters of its monitor's work area, fill,
// centered, or back to where it was. A full-screen tile is a half.
func pressMenu(_ int, identifier string) error {
	var tile string
	for name, id := range tiles {
		if id == identifier {
			tile = name
		}
	}
	w, isApp, ok := foreground()
	if !ok || !isApp {
		return errors.New("No window is in front.")
	}
	var frame, work [4]C.int32_t
	if C.wd_frame(C.uint32_t(w.id), &frame[0], &work[0]) != 0 {
		return errors.New("That window has closed.")
	}
	x, y, width, height := int32(work[0]), int32(work[1]), int32(work[2]), int32(work[3])
	halfW, halfH := width/2, height/2
	at := map[string][4]int32{
		"left": {x, y, halfW, height}, "right": {x + halfW, y, width - halfW, height},
		"top": {x, y, width, halfH}, "bottom": {x, y + halfH, width, height - halfH},
		"top_left": {x, y, halfW, halfH}, "top_right": {x + halfW, y, width - halfW, halfH},
		"bottom_left": {x, y + halfH, halfW, height - halfH}, "bottom_right": {x + halfW, y + halfH, width - halfW, height - halfH},
		"fill":              {x, y, width, height},
		"full_screen_left":  {x, y, halfW, height},
		"full_screen_right": {x + halfW, y, width - halfW, height},
	}
	fw, fh := min(int32(frame[2]), width), min(int32(frame[3]), height)
	at["center"] = [4]int32{x + (width-fw)/2, y + (height-fh)/2, fw, fh}
	untiledMu.Lock()
	if prev, ok := untiled[w.id]; ok {
		at["previous"] = prev
	}
	if tile != "previous" {
		untiled[w.id] = [4]int32{int32(frame[0]), int32(frame[1]), int32(frame[2]), int32(frame[3])}
	}
	untiledMu.Unlock()
	r, ok := at[tile]
	if !ok {
		return errors.New("That window has no earlier place.")
	}
	C.wd_place(C.uint32_t(w.id), C.int32_t(r[0]), C.int32_t(r[1]), C.int32_t(r[2]), C.int32_t(r[3]))
	return nil
}

// icon draws an app's icon as a PNG px wide, from the Start menu's entry
// or the program.
func icon(bundle string, px int) []byte {
	setup()
	pixels := make([]byte, px*px*4)
	runtime.LockOSThread()
	c := C.CString(bundle)
	failed := C.wd_icon(c, C.int(px), (*C.uint8_t)(unsafe.Pointer(&pixels[0]))) != 0
	C.free(unsafe.Pointer(c))
	runtime.UnlockOSThread()
	if failed {
		return nil
	}
	// The shell's premultiplied BGRA, as Go's premultiplied RGBA.
	img := image.NewRGBA(image.Rect(0, 0, px, px))
	for i := 0; i < len(pixels); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = pixels[i+2], pixels[i+1], pixels[i], pixels[i+3]
	}
	var out bytes.Buffer
	if png.Encode(&out, img) != nil {
		return nil
	}
	return out.Bytes()
}

// quit asks each of the app's windows to close, as their close buttons do:
// one with unsaved work asks on the PC first.
func quit(bundle string) {
	for _, w := range listWindows() {
		if sameApp(w.key(), bundle) {
			C.wd_close(C.uint32_t(w.id))
		}
	}
}

func raise(_ int, window uint32) { C.wd_raise(C.uint32_t(window)) }

// pidOf isn't needed on Windows: raise takes the window alone.
func pidOf(uint32) int { return 0 }

func watch() { C.wd_watch() }

//export goChanged
func goChanged() { changed() }

// swipe does nothing: Windows has no spaces to swipe between.
func swipe(int, float64, float64) {}

// Open brings the app's front window forward, restored if minimized, as a
// click on its taskbar button does; an app with no window opens. Windows
// may open it behind the front window, so its first window is brought
// forward once it shows.
func Open(_ context.Context, bundle string) error {
	if bundle == "" || len(bundle) > 1024 || strings.ContainsAny(bundle, "\x00\r\n") {
		return errors.New("No app with that ID.")
	}
	before := map[uint32]bool{}
	for _, w := range listWindows() {
		if sameApp(w.key(), bundle) {
			raise(0, w.id)
			return nil
		}
		before[w.id] = true
	}
	setup()
	runtime.LockOSThread()
	c := C.CString(bundle)
	failed := C.wd_launch(c) != 0
	C.free(unsafe.Pointer(c))
	runtime.UnlockOSThread()
	if failed {
		return errors.New("Windows couldn't open that app.")
	}
	go func() {
		for range 25 {
			time.Sleep(200 * time.Millisecond)
			for _, w := range listWindows() {
				if !before[w.id] && sameApp(w.key(), bundle) {
					raise(0, w.id)
					return
				}
			}
		}
	}()
	return nil
}

// notApps are Start menu entries that open a document or a web page.
var notApps = []string{".url", ".txt", ".chm", ".htm", ".html", ".pdf", ".rtf", ".log", ".ini", ".lnk"}

// Installed are the apps the Start menu's All apps lists, by name, with
// the uninstallers and documents left out.
func Installed() []App {
	setup()
	runtime.LockOSThread()
	p := C.wd_apps()
	runtime.UnlockOSThread()
	if p == nil {
		return nil
	}
	list := C.GoString(p)
	C.free(unsafe.Pointer(p))
	seen := map[string]bool{}
	var out []App
	for line := range strings.SplitSeq(list, "\n") {
		name, id, ok := strings.Cut(line, "\t")
		lower := strings.ToLower(id)
		if !ok || name == "" || id == "" || seen[id] || strings.Contains(strings.ToLower(name), "uninstall") ||
			slices.ContainsFunc(notApps, func(ext string) bool { return strings.HasSuffix(lower, ext) }) {
			continue
		}
		seen[id] = true
		out = append(out, App{Bundle: id, Name: name})
	}
	slices.SortFunc(out, func(a, b App) int { return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
	return out
}
