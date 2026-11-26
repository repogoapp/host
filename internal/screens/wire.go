package screens

import (
	"strconv"

	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/screens/desktop"
)

// The wire's enums. Each lists its values, so the phones get an enum with
// a case for a value a newer host adds.

// Kind is what a screen is.
type Kind string

const (
	KindSimulator Kind = "simulator"
	KindEmulator  Kind = "emulator"
	KindDisplay   Kind = "display"
)

func (Kind) Values() []string { return []string{"simulator", "emulator", "display"} }

// Platform is a screen's operating system.
type Platform string

const (
	PlatformIOS     Platform = "ios"
	PlatformAndroid Platform = "android"
	PlatformMacOS   Platform = "macos"
	PlatformWindows Platform = "windows"
)

func (Platform) Values() []string { return []string{"ios", "android", "macos", "windows"} }

// ScreenState is whether a screen is running, or on its way.
type ScreenState string

const (
	StateShutdown     ScreenState = "shutdown"
	StateBooting      ScreenState = "booting"
	StateBooted       ScreenState = "booted"
	StateShuttingDown ScreenState = "shutting_down"
)

func (ScreenState) Values() []string {
	return []string{"shutdown", "booting", "booted", "shutting_down"}
}

// Form is a screen's shape, for its icon and the viewer's frame before
// the first video frame.
type Form string

const (
	FormPhone    Form = "phone"
	FormTablet   Form = "tablet"
	FormFoldable Form = "foldable"
	FormDesktop  Form = "desktop"
)

func (Form) Values() []string { return []string{"phone", "tablet", "foldable", "desktop"} }

// Appearance is light or dark mode.
type Appearance string

const (
	AppearanceLight Appearance = "light"
	AppearanceDark  Appearance = "dark"
)

func (Appearance) Values() []string { return []string{"light", "dark"} }

// ContentSize is an iOS content size category; Android takes each as a font scale.
type ContentSize string

func (ContentSize) Values() []string {
	out := make([]string, len(contentSizes))
	for i, s := range contentSizes {
		out[i] = s.name
	}
	return out
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

func (s ContentSize) valid() bool {
	for _, c := range contentSizes {
		if c.name == string(s) {
			return true
		}
	}
	return false
}

// Pose is a foldable's: closed, open, book or laptop.
type Pose string

const (
	PoseClosed Pose = "closed"
	PoseOpen   Pose = "open"
	PoseBook   Pose = "book"
	PoseLaptop Pose = "laptop"
)

func (Pose) Values() []string { return []string{"closed", "open", "book", "laptop"} }

// Turn is a quarter rotation of the device.
type Turn string

const (
	TurnLeft  Turn = "left"
	TurnRight Turn = "right"
)

func (Turn) Values() []string { return []string{"left", "right"} }

// Control is a screens.* action a screen takes; the phone shows only these.
type Control string

const (
	ControlAppearance       Control = "appearance"
	ControlContentSize      Control = "content_size"
	ControlContrast         Control = "contrast"
	ControlStatusBar        Control = "status_bar"
	ControlSlowAnimations   Control = "slow_animations"
	ControlOnscreenKeyboard Control = "onscreen_keyboard"
	ControlPose             Control = "pose"
	ControlRotate           Control = "rotate"
	ControlOpenURL          Control = "open_url"
	ControlLocation         Control = "location"
	ControlPush             Control = "push"
	ControlShake            Control = "shake"
	ControlMemoryWarning    Control = "memory_warning"
	ControlScreenshot       Control = "screenshot"
	// ControlRecord is screens.recording.start and stop.
	ControlRecord Control = "record"
	// ControlText is typing on /stream; ControlDesktop the Mac's
	// screens.desktop.* methods.
	ControlText    Control = "text"
	ControlDesktop Control = "desktop"
)

func (Control) Values() []string {
	return []string{"appearance", "content_size", "contrast", "status_bar", "slow_animations",
		"onscreen_keyboard", "pose", "rotate", "open_url", "location", "push", "shake",
		"memory_warning", "screenshot", "record", "text", "desktop"}
}

// HardwareButton is a device button pressed over /stream.
type HardwareButton string

const (
	ButtonHome        HardwareButton = "home"
	ButtonBack        HardwareButton = "back"
	ButtonRecents     HardwareButton = "recents"
	ButtonAppSwitcher HardwareButton = "app_switcher"
	ButtonLock        HardwareButton = "lock"
	ButtonSiri        HardwareButton = "siri"
	ButtonSide        HardwareButton = "side"
	ButtonVolumeUp    HardwareButton = "volume_up"
	ButtonVolumeDown  HardwareButton = "volume_down"
	ButtonMute        HardwareButton = "mute"
)

func (HardwareButton) Values() []string {
	return []string{"home", "back", "recents", "app_switcher", "lock", "siri", "side",
		"volume_up", "volume_down", "mute"}
}

// WindowButton is a Mac window's title-bar button.
type WindowButton string

func (WindowButton) Values() []string {
	return []string{desktop.ButtonClose, desktop.ButtonMinimize, desktop.ButtonFullScreen}
}

// Tile is a Mac window's Move & Resize item.
type Tile string

func (Tile) Values() []string {
	return []string{"left", "right", "top", "bottom", "top_left", "top_right", "bottom_left",
		"bottom_right", "fill", "center", "previous", "full_screen_left", "full_screen_right"}
}

// OS is a screen's operating system and version.
type OS struct {
	Name    string `json:"name"`    // iOS, Android, macOS
	Version string `json:"version"` // 27.1; Android's release (16), empty when its API level is new to the host
	// APILevel is an Android emulator's.
	APILevel *int `json:"api_level,omitempty"`
}

// State is a device's settings as the phone shows them. A field is absent
// when the device doesn't have the setting or the host hasn't read it.
type State struct {
	Appearance     *Appearance  `json:"appearance,omitempty"`
	ContentSize    *ContentSize `json:"content_size,omitempty"`
	Contrast       *bool        `json:"contrast,omitempty"`
	StatusBar      *bool        `json:"status_bar,omitempty"` // 9:41, full battery and signal
	SlowAnimations *bool        `json:"slow_animations,omitempty"`
	// Pose is a foldable's; Android reports only closed and open.
	Pose *Pose `json:"pose,omitempty"`
	// OnscreenKeyboard is whether iOS shows its own keyboard for a focused
	// field, which it does only with no hardware keyboard connected.
	OnscreenKeyboard *bool `json:"onscreen_keyboard,omitempty"`
}

// ViewChanged is a screen's live view, for room screen:<id>: the actions it
// takes, its buttons, and its settings. Sent whole on any change.
type ViewChanged struct {
	Screen   string           `json:"screen"`
	Controls []Control        `json:"controls" wire:"array"`
	Buttons  []HardwareButton `json:"buttons" wire:"array"`
	// State is absent for a Mac display, which has none.
	State *State `json:"state,omitempty"`
	// Recording is whether a recording runs, or one that reached its limit
	// waits for screens.recording.stop to collect it.
	Recording bool `json:"recording,omitempty"`
	emit.Stamp
}

func (ViewChanged) Method() string   { return "screens.view_changed" }
func (ViewChanged) StateKey() string { return "view" }
func (ViewChanged) Retain() bool     { return true }
func (v ViewChanged) Stamped(s emit.Stamp) emit.Stateful {
	v.Stamp = s
	return v
}

// DesktopChanged is a Mac display's spaces, Dock and focused window, for
// room screen:<id>, sent while it streams and the desktop changes.
type DesktopChanged struct {
	Screen  string      `json:"screen"`
	Desktop DesktopView `json:"desktop"`
	emit.Stamp
}

func (DesktopChanged) Method() string   { return "screens.desktop_changed" }
func (DesktopChanged) StateKey() string { return "desktop" }
func (DesktopChanged) Retain() bool     { return true }
func (d DesktopChanged) Stamped(s emit.Stamp) emit.Stateful {
	d.Stamp = s
	return d
}

func init() { emit.Register(ViewChanged{}, DesktopChanged{}) }

// DesktopView is a Mac's desktop as the phone draws it. Space IDs are
// decimal strings: the phones have no unsigned 64-bit integer.
type DesktopView struct {
	// Locked is set while the session is locked, and nothing else is.
	Locked bool `json:"locked,omitempty"`
	// Spaces are the main display's, in swipe order.
	Spaces       []SpaceView `json:"spaces" wire:"array"`
	CurrentSpace string      `json:"current_space,omitempty"`
	// Apps are the Dock's, once each, in its order.
	Apps  []AppView  `json:"apps" wire:"array"`
	Front string     `json:"front,omitempty"`
	Focus *FocusView `json:"focus,omitempty"`
}

type SpaceView struct {
	ID string `json:"id"`
	// Apps are a full-screen space's apps by bundle ID; empty for a desktop.
	Apps []string `json:"apps" wire:"array"`
}

type AppView struct {
	Bundle  string       `json:"bundle"`
	Name    string       `json:"name"`
	Running bool         `json:"running,omitempty"`
	Windows []WindowView `json:"windows" wire:"array"`
}

type WindowView struct {
	ID int `json:"id"`
	// Title is empty without the Screen Recording permission.
	Title string `json:"title,omitempty"`
	// Space is absent when the Mac doesn't say.
	Space *string `json:"space,omitempty"`
}

type FocusView struct {
	// Window is absent without Accessibility; the buttons then work as
	// keys on the focused window.
	Window     *int           `json:"window,omitempty"`
	Title      string         `json:"title,omitempty"`
	FullScreen bool           `json:"full_screen,omitempty"`
	Buttons    []WindowButton `json:"buttons" wire:"array"`
	// Tiles is whether the window takes Move & Resize.
	Tiles bool `json:"tiles,omitempty"`
}

// App is one installed app, for the Launchpad.
type App struct {
	Bundle string `json:"bundle"`
	Name   string `json:"name"`
}

// Icon is an app's icon as a PNG.
type Icon struct {
	Bundle string `json:"bundle"`
	Image  []byte `json:"image"`
}

func spaceID(id uint64) string { return strconv.FormatUint(id, 10) }

// viewOf is the desktop as the phone draws it.
func viewOf(d desktop.Desktop) DesktopView {
	v := DesktopView{Locked: d.Locked, Spaces: []SpaceView{}, Apps: []AppView{}, Front: d.Front}
	for i, s := range d.Spaces {
		apps := s.Apps
		if apps == nil {
			apps = []string{}
		}
		v.Spaces = append(v.Spaces, SpaceView{ID: spaceID(s.ID), Apps: apps})
		if i == d.Current {
			v.CurrentSpace = spaceID(s.ID)
		}
	}
	for _, a := range d.Apps {
		app := AppView{Bundle: a.Bundle, Name: a.Name, Running: a.Running, Windows: []WindowView{}}
		for _, w := range a.Windows {
			win := WindowView{ID: int(w.ID), Title: w.Title}
			if w.Space != 0 {
				id := spaceID(w.Space)
				win.Space = &id
			}
			app.Windows = append(app.Windows, win)
		}
		v.Apps = append(v.Apps, app)
	}
	if f := d.Focus; f != nil {
		focus := &FocusView{Title: f.Title, FullScreen: f.FullScreen, Tiles: f.Tiles, Buttons: []WindowButton{}}
		if f.Window != 0 {
			w := int(f.Window)
			focus.Window = &w
		}
		for _, b := range f.Buttons {
			focus.Buttons = append(focus.Buttons, WindowButton(b))
		}
		v.Focus = focus
	}
	return v
}

// ScreenError is the code an error carries for the phone to act on.
type ScreenError string

const (
	ErrorAuthRequired       ScreenError = "auth_required"
	ErrorAuthRejected       ScreenError = "auth_rejected"
	ErrorNotRunning         ScreenError = "not_running"
	ErrorNotStreaming       ScreenError = "not_streaming"
	ErrorUnsupported        ScreenError = "unsupported"
	ErrorUnsupportedVersion ScreenError = "unsupported_version"
)

func (ScreenError) Values() []string {
	return []string{"auth_required", "auth_rejected", "not_running", "not_streaming", "unsupported",
		"unsupported_version"}
}

// Error is a failure with a code: on a call, in error.data; on /stream, in
// its failed message.
type Error struct {
	Code    ScreenError
	Message string
	kind    errkind.Kind
}

func (e *Error) Error() string { return e.Message }

// Unwrap is the error's kind, so the router maps it as any kinded error.
func (e *Error) Unwrap() error { return errkind.New(e.kind, e.Message) }

// Is matches the same code, so errors.Is(err, ErrNotStreaming) holds for a copy.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code && t.Message == e.Message
}

// ErrorData is error.data on a call that fails with one.
type ErrorData struct {
	Code ScreenError `json:"code"`
}

func (e *Error) ErrorData() any { return ErrorData{Code: e.Code} }

// StreamCode is the code /stream's failed message carries.
func (e *Error) StreamCode() string { return string(e.Code) }

func newError(kind errkind.Kind, code ScreenError, message string) *Error {
	return &Error{Code: code, Message: message, kind: kind}
}

var (
	ErrNotRunning   = newError(errkind.Unavailable, ErrorNotRunning, "The screen isn't running.")
	ErrNotStreaming = newError(errkind.Unavailable, ErrorNotStreaming, "Open the screen's live view first.")
	ErrUnsupported  = newError(errkind.Invalid, ErrorUnsupported, "This screen can't do that.")
)
