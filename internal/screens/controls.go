package screens

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/repogoapp/host/internal/screens/capture"
)

// Message is a text message the host sends on a live view's /stream: what
// the device can do and its settings (first, and after each change), a
// screenshot, or why an action didn't happen.
type Message struct {
	Controls   []string `json:"controls,omitempty"`
	State      *State   `json:"state,omitempty"`
	Screenshot string   `json:"screenshot,omitempty"` // base64 JPEG or PNG
	Failed     string   `json:"failed,omitempty"`
}

// State is the device's settings as the phone shows them.
type State struct {
	Appearance     string `json:"appearance"`   // light or dark
	ContentSize    string `json:"content_size"` // an iOS content size category, as contentSizes names them
	Contrast       bool   `json:"contrast"`
	Orientation    int    `json:"orientation"` // UIDeviceOrientation: 1 portrait, 2 upside down, 3 landscape left, 4 landscape right
	Rotation       int    `json:"rotation"`    // degrees the phone turns the video to show it upright
	StatusBar      bool   `json:"status_bar"`  // 9:41, full battery and signal
	SlowAnimations bool   `json:"slow_animations"`
	Folded         bool   `json:"folded"`
	// Pose is a foldable iPhone's: closed, open, book or laptop; empty until set.
	Pose string `json:"pose"`
	// OnscreenKeyboard is whether iOS shows its own keyboard for a focused
	// field, which it does only with no hardware keyboard connected.
	OnscreenKeyboard bool `json:"onscreen_keyboard"`
}

// contentSizes are iOS's content size categories, smallest first, with the
// Android font scale each stands for.
var contentSizes = []struct {
	name  string
	scale float64
}{
	{"extra-small", 0.82}, {"small", 0.88}, {"medium", 0.94}, {"large", 1.0},
	{"extra-large", 1.12}, {"extra-extra-large", 1.24}, {"extra-extra-extra-large", 1.35},
	{"accessibility-medium", 1.5}, {"accessibility-large", 1.65}, {"accessibility-extra-large", 1.8},
	{"accessibility-extra-extra-large", 1.9}, {"accessibility-extra-extra-extra-large", 2.0},
}

// controller carries out one live view's actions on its device, turning
// each into the platform's command, and keeps the state it reports.
type controller struct {
	d     Screen
	adb   string
	reply func(any) error

	mu    sync.Mutex
	state State
	// foldable is set when simcap reports two panels, which adds fold.
	foldable bool
	// displayed is set once simcap reports the panel's turn, which then
	// decides Rotation rather than the orientation the host asked for.
	displayed bool
	// restart asks an Android recorder to start again, and sizeChanged its
	// touches to read the screen again: a rotation or fold changed its size.
	restart     chan struct{}
	sizeChanged atomic.Bool
}

func newController(ctx context.Context, d Screen, reply func(any) error) *controller {
	c := &controller{d: d, reply: reply, restart: make(chan struct{}, 1), state: State{Orientation: 1}}
	switch d.Platform {
	case "ios":
		c.readIOS(ctx)
	case "android":
		c.adb = androidTool("platform-tools", "adb")
		c.readAndroid(ctx)
	}
	return c
}

// controls are the actions this device takes, by name; the phone shows only these.
func (c *controller) controls() []string {
	if c.d.Kind == KindDisplay {
		// The screen takes clicks, drags and typing; nothing else yet.
		return []string{"text"}
	}
	common := []string{"home", "appearance", "content_size", "contrast", "rotate", "url", "location",
		"status_bar", "slow_animations", "screenshot"}
	if c.d.Platform == "android" {
		out := append(common, "back", "recents", "lock", "volume_up", "volume_down", "text")
		if c.hasHinge() {
			out = append(out, "fold")
		}
		return out
	}
	out := append(common, "paste", "push", "shake", "memory_warning")
	c.mu.Lock()
	if c.foldable {
		out = append(out, "fold")
	}
	c.mu.Unlock()
	// The on-screen keyboard is a CoreSimulator setting: every runtime takes it.
	out = append(out, "keyboard")
	switch {
	case !c.universalHID():
		out = append(out, "text", "lock", "siri", "volume_up", "volume_down")
	case capture.UniversalHID:
		// iOS 27 takes keys through Universal HID; its buttons are still
		// Indigo messages, which it ignores.
		out = append(out, "text")
	}
	return out
}

// sendState reports the current state to the phone.
func (c *controller) sendState() {
	c.mu.Lock()
	state := c.state
	c.mu.Unlock()
	c.reply(Message{State: &state})
}

// hostRuns reports whether the host carries out in itself, rather than the
// device's input path (simcap on iOS, the adb shell on Android).
func (c *controller) hostRuns(in capture.Input) bool {
	if c.d.Kind == KindDisplay {
		return false
	}
	if in.Key != nil {
		// Keys go in order with typing and touches, on the device's path.
		return false
	}
	if in.Action != "" {
		if c.d.Platform == "ios" {
			return in.Action != capture.ActionText &&
				in.Action != capture.ActionMemoryWarning
		}
		// Typing goes in order with touches, through the adb shell.
		return in.Action != capture.ActionText
	}
	// iOS 26 and later drop the Home button's HID press.
	return c.d.Platform == "ios" && in.Button == "home" && iosMajor(c.d.OS) >= 26
}

// act carries out one action the host runs, then reports the state. forward
// passes an action on to simcap (iOS rotation, after the host decides the
// orientation).
func (c *controller) act(ctx context.Context, in capture.Input, forward func(capture.Input) error) error {
	var err error
	if c.d.Platform == "ios" {
		err = c.actIOS(ctx, in, forward)
	} else {
		err = c.actAndroid(ctx, in)
	}
	if err == nil && in.Action != "screenshot" {
		c.sendState()
	}
	return err
}

func (c *controller) set(change func(*State)) {
	c.mu.Lock()
	change(&c.state)
	c.mu.Unlock()
}

// fromSimcap takes a message simcap sent the host: the active panel and its
// turn, or why an action failed.
func (c *controller) fromSimcap(message []byte) {
	var m struct {
		Display *struct {
			Screen   uint32 `json:"screen"`
			Rotation int    `json:"rotation"`
			Foldable bool   `json:"foldable"`
		} `json:"display"`
		Failed string `json:"failed"`
	}
	if json.Unmarshal(message, &m) != nil {
		return
	}
	if m.Failed != "" {
		c.reply(Message{Failed: m.Failed})
		return
	}
	if m.Display == nil {
		return
	}
	c.mu.Lock()
	learnedFoldable := m.Display.Foldable && !c.foldable
	c.foldable = c.foldable || m.Display.Foldable
	c.displayed = true
	c.state.Rotation = upright(m.Display.Rotation)
	c.mu.Unlock()
	if learnedFoldable {
		c.reply(Message{Controls: c.controls()})
	}
	c.sendState()
}

func lightOrDark(v string) string {
	if v == "dark" {
		return "dark"
	}
	return "light"
}

func contentSize(name string) (string, bool) {
	for _, s := range contentSizes {
		if s.name == name {
			return name, true
		}
	}
	return "", false
}

// shellQuote quotes s for the device's sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
