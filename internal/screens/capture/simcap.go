// Package simcap is `repogo simcap`: a booted iOS simulator's screen as H.264
// on stdout, one frame per message (a 4-byte big-endian length, the capture
// time in microseconds, then the access unit), and input from stdin, through Xcode's private
// SimulatorKit. The host runs it as a child process for each live view, so a
// SimulatorKit that changed under an Xcode update can crash only that view.
package capture

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// tellHost writes a message for the host beside the frames.
func tellHost(message map[string]any) {
	if b, err := json.Marshal(message); err == nil {
		tell(b)
	}
}

// Input is one line the host writes a capture child: a touch, its point a
// fraction of the screen as the frames show it (0 to 1 from the top left), a
// button, a key, or an action. An action with an ID is answered {"done":{"id","error"}}.
type Input struct {
	ID     int     `json:"id,omitempty"`
	Touch  string  `json:"touch,omitempty"` // down, move, up or cancel (the phone took the finger for a pinch)
	X      float64 `json:"x,omitempty"`
	Y      float64 `json:"y,omitempty"`
	Button string  `json:"button,omitempty"` // home, back, recents, lock, siri, volume_up, volume_down
	// Action is one of the Action constants below; Value carries its argument.
	Action string `json:"action,omitempty"`
	Value  string `json:"value,omitempty"`

	// A Mac screen's trackpad and keyboard: the cursor where the phone keeps
	// it with the buttons held, wheel notches at it, one key pressed or released.
	Pointer *Pointer `json:"pointer,omitempty"`
	Scroll  *Scroll  `json:"scroll,omitempty"`
	Key     *Key     `json:"key,omitempty"`

	// Credit is how many frames the encoder may make now, replacing the
	// last credit: the host's pacer sends 1 when the phone can take
	// another. Until the first, every frame is encoded.
	Credit int `json:"credit,omitempty"`
	// Bitrate is the encoder's target in bits a second, as the host's pacer
	// finds the link allows.
	Bitrate int `json:"bitrate,omitempty"`
}

// Pointer is the cursor at a fraction of the screen, with buttons held: bit
// 0 the left button, 2 the right.
type Pointer struct {
	X       float64 `json:"x"`
	Y       float64 `json:"y"`
	Buttons byte    `json:"buttons"`
}

// Scroll is wheel notches at the cursor: positive DY scrolls the content
// down into view from above (wheel up), positive DX from the left.
type Scroll struct {
	DX int `json:"dx"`
	DY int `json:"dy"`
}

// Key is one X11 keysym pressed or released.
type Key struct {
	Keysym uint32 `json:"keysym"`
	Down   bool   `json:"down"`
}

// Valid reports whether in is a touch with its point on the screen, a button
// or an action.
func (in Input) Valid() bool {
	switch in.Touch {
	case "down", "move", "up", "cancel":
		return in.X >= 0 && in.X <= 1 && in.Y >= 0 && in.Y <= 1
	case "":
		if p := in.Pointer; p != nil {
			return p.X >= 0 && p.X <= 1 && p.Y >= 0 && p.Y <= 1
		}
		return in.Button != "" || in.Action != "" || in.Scroll != nil || in.Key != nil || in.Credit > 0 || in.Bitrate > 0
	}
	return false
}

// The actions a capture child carries out; the host runs the rest itself.
const (
	ActionRotate         = "rotate"          // Value: a UIDeviceOrientation, 1 to 4
	ActionKeyboard       = "keyboard"        // Value: on or off, the on-screen keyboard
	ActionPasteKeys      = "paste_keys"      // ⌘V, after the host set the device's pasteboard
	ActionText           = "text"            // types Value
	ActionMemoryWarning  = "memory_warning"  //
	ActionSlowAnimations = "slow_animations" // Value: on or off
	ActionFold           = "fold"            // Value: closed, open, book or laptop (a foldable's pose)
	ActionAppSwitcher    = "app_switcher"    // the app switcher; Value SwitcherPress presses Home twice, else Home twice through dtuhidd, else the swipe
	ActionKeyframe       = "keyframe"        // the next frame a keyframe, sent even if the screen is still
	ActionTurn           = "turn"            // Value: quarter turns clockwise, 0 to 3, that show the framebuffer upright
	ActionElements       = "elements"        // what's on the screen by label, answered as the done message's data
)

// Typeable reports whether a US keyboard has a key for r: the rest is
// typed through the pasteboard.
func Typeable(r rune) bool {
	_, _, ok := keyFor(r)
	return ok
}

// onscreen is whether the on-screen keyboard was last turned on.
var onscreen bool

// keyboardReady is set once this session connected the hardware keyboard
// the phone's typing goes through: iOS counts key presses only from one,
// and a device may have been left with it off.
var keyboardReady bool

// withKeyboard runs typing that needs a connected hardware keyboard, as
// iOS counts key presses only from one: with the device showing its own
// keyboard, one is connected for the typing, then the on-screen one comes back.
func withKeyboard(typing func()) {
	if !onscreen {
		readyKeyboard()
		typing()
		return
	}
	onscreenKeyboard(false)
	time.Sleep(keyboardSettle)
	typing()
	time.Sleep(keyboardSettle)
	onscreenKeyboard(true)
}

// readyKeyboard connects that keyboard once, unless the device is set to
// show its own, and waits for iOS to take it.
func readyKeyboard() {
	if keyboardReady || onscreen {
		return
	}
	keyboardReady = true
	if onscreenKeyboard(false) {
		time.Sleep(keyboardSettle)
	}
}

// keyboardSettle is how long iOS takes to take keys from a hardware keyboard
// just connected, and to finish them before it is disconnected.
const keyboardSettle = 400 * time.Millisecond

// Indigo button sources and the HID pages and usages DeviceKit's chrome.json
// gives the hardware buttons.
var buttons = map[string]func(){
	"home":        func() { button(0x0, 0) },
	"lock":        func() { button(0x1, 0) },
	"siri":        func() { button(0x400002, 300) }, // a tap is ignored; a hold invokes it
	"volume_up":   func() { hid(12, 233) },
	"volume_down": func() { hid(12, 234) },
}

// SwitcherPress is ActionAppSwitcher's Value for the Home button's double
// press, where the device takes it.
const SwitcherPress = "press"

// Run streams udid's screen to out and plays in's lines on it until in ends;
// universal sends input through Universal HID (iOS 27), and headless sends no
// frames. Beside the frames, out carries JSON for the host, its length's top
// bit set: {"display":{"screen","rotation","foldable"}} as the panel or its
// turn changes, and {"done":{"id","error","data"}} for an action with an ID.
func Run(developerDir, udid string, universal, headless bool, out *os.File, in io.Reader) error {
	if err := start(developerDir, udid, universal, headless, out); err != nil {
		return err
	}
	defer stop()
	var finger fingerState
	// A viewer that closes mid-touch (a swipe away to the chat) never lifts
	// its finger; left down, the simulator reads every later touch as a second.
	defer finger.lift()
	lines := bufio.NewScanner(in)
	for lines.Scan() {
		var input Input
		if json.Unmarshal(lines.Bytes(), &input) != nil || !input.Valid() {
			continue
		}
		if input.Credit > 0 || input.Bitrate > 0 {
			pace(input)
			continue
		}
		finger.track(input)
		var data json.RawMessage
		var err error
		if input.Action == ActionElements {
			if data = elements(); data == nil {
				err = fmt.Errorf("this simulator's screen can't be read by label")
			}
		} else {
			err = play(input)
		}
		if input.ID != 0 {
			done := map[string]any{"id": input.ID}
			if err != nil {
				done["error"] = err.Error()
			}
			if data != nil {
				done["data"] = data
			}
			tellHost(map[string]any{"done": done})
		}
	}
	return lines.Err()
}

// fingerState is whether the simulator has a finger down, and where, so a
// lost lift is made up before the next press and when the input ends.
type fingerState struct {
	down bool
	x, y float64
}

func (f *fingerState) track(in Input) {
	switch in.Touch {
	case "down":
		f.lift()
		f.down, f.x, f.y = true, in.X, in.Y
	case "move":
		f.x, f.y = in.X, in.Y
	case "up", "cancel":
		f.down = false
	}
}

func (f *fingerState) lift() {
	if f.down {
		touchAt(2, f.x, f.y)
		f.down = false
	}
}

// turns is the quarter turns, clockwise, the frames are turned by: what the
// host last sent as ActionTurn.
var turns atomic.Int32

// touchAt touches a point of the screen as the frames show it, at its place
// in the framebuffer, which stays in the device's portrait.
func touchAt(phase int, x, y float64) {
	x, y = framebufferPoint(x, y, int(turns.Load()))
	touch(phase, x, y)
}

// framebufferPoint is where a point of a frame turned clockwise by quarter
// turns lies in the framebuffer, both as fractions from the top left.
func framebufferPoint(x, y float64, turns int) (float64, float64) {
	switch turns {
	case 1:
		return y, 1 - x
	case 2:
		return 1 - x, 1 - y
	case 3:
		return 1 - y, x
	}
	return x, y
}

func play(in Input) error {
	switch in.Touch {
	case "down":
		touchAt(0, in.X, in.Y)
	case "move":
		touchAt(1, in.X, in.Y)
	case "up", "cancel":
		// Indigo touches have no cancel; a lift ends it.
		touchAt(2, in.X, in.Y)
	}
	if press, ok := buttons[in.Button]; ok {
		press()
	}
	if in.Key != nil {
		readyKeyboard()
		// A key from the phone: special keys and modifiers as pressed and
		// released, so ⇧ or ⌘ held from its row reaches the next key.
		if usage, ok := usageFor(in.Key.Keysym); ok {
			key(usage, in.Key.Down)
		}
	}
	switch in.Action {
	case ActionRotate:
		var orientation int
		if _, err := fmt.Sscan(in.Value, &orientation); err != nil || !rotate(orientation) {
			return fmt.Errorf("couldn't rotate: open Simulator on the Mac")
		}
	case ActionFold:
		if !fold(in.Value) {
			return fmt.Errorf("couldn't fold to %s", in.Value)
		}
	case ActionKeyboard:
		if !onscreenKeyboard(in.Value == "on") {
			return fmt.Errorf("couldn't change the on-screen keyboard")
		}
		onscreen = in.Value == "on"
		keyboardReady = !onscreen
	case ActionText:
		withKeyboard(func() { typeText(in.Value) })
	case ActionPasteKeys:
		// ⌘V into the focused field, the pasteboard already set by the host.
		withKeyboard(func() {
			key(commandKey, true)
			key(vKey, true)
			key(vKey, false)
			key(commandKey, false)
		})
	case ActionMemoryWarning:
		memoryWarning()
	case ActionAppSwitcher:
		switch {
		case in.Value == SwitcherPress:
			doubleHome()
		case !serviceDoubleHome():
			appSwitcher()
		}
	case ActionSlowAnimations:
		caDebug("debug_slow_animations", in.Value == "on")
	case ActionKeyframe:
		keyframe()
	case ActionTurn:
		if n, err := strconv.Atoi(in.Value); err == nil && n >= 0 && n <= 3 {
			turns.Store(int32(n))
			turn(n)
		}
	}
	return nil
}

// doubleHome opens the app switcher as Simulator's own does: Home twice,
// which iOS 26 and later ignore.
func doubleHome() {
	buttons["home"]()
	time.Sleep(120 * time.Millisecond)
	buttons["home"]()
}

// Home's HID page and usage (Consumer, Menu), as dtuhidd takes the button.
const consumerPage, menuUsage = 0x0C, 0x40

// serviceDoubleHome opens the app switcher with Home pressed twice through
// dtuhidd, which takes the press where iOS 26 and later drop Indigo's. False,
// pressing nothing, where the simulator has no dtuhidd.
func serviceDoubleHome() bool {
	if !serviceButton(consumerPage, menuUsage, 50) {
		return false
	}
	time.Sleep(120 * time.Millisecond)
	serviceButton(consumerPage, menuUsage, 50)
	return true
}

// appSwitcher opens the app switcher with a slow swipe up from the bottom
// of the upright screen that pauses before it lifts; a quicker or shorter
// one went home on iOS 27's Duo.
func appSwitcher() {
	const from, to, steps = 0.995, 0.60, 50
	at := func(phase int, y float64) { touchAt(phase, 0.5, y) }
	at(0, from)
	for i := 1; i <= steps; i++ {
		time.Sleep(16 * time.Millisecond)
		at(1, from-(from-to)*float64(i)/steps)
	}
	time.Sleep(600 * time.Millisecond)
	at(2, to)
}

// typeText presses each character's key, with Shift where it needs one.
// Characters a US keyboard has no key for are skipped.
func typeText(text string) {
	for _, r := range text {
		usage, shift, ok := keyFor(r)
		if !ok {
			continue
		}
		if shift {
			key(leftShift, true)
		}
		key(usage, true)
		key(usage, false)
		if shift {
			key(leftShift, false)
		}
		time.Sleep(8 * time.Millisecond)
	}
}
