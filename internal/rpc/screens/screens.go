// Package screens is the `screens.*` methods, and the /stream request that
// shows one: this machine's simulators, emulators and its display. The tools
// are driven in internal/screens.
package screens

import (
	"context"
	"encoding/json"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/rpc"
	core "github.com/repogoapp/host/internal/screens"
)

type Deps struct {
	Screens *core.Manager
	// PhoneName is what a paired phone is called, for its mic on the Mac.
	PhoneName func(device.ID) string
}

type ListResult struct {
	Screens []core.Screen `json:"screens" wire:"array"`
}

// ScreenParams names a screen by the id screens.list gave it.
type ScreenParams struct {
	Screen string `json:"screen"`
}

type SetParams struct {
	Screen string `json:"screen"`
	core.Settings
}

type RotateParams struct {
	Screen string    `json:"screen"`
	Turn   core.Turn `json:"turn"`
}

type OpenURLParams struct {
	Screen string `json:"screen"`
	URL    string `json:"url"`
}

type LocationParams struct {
	Screen string `json:"screen"`
	// Coordinate absent clears the location.
	Coordinate *core.Coordinate `json:"coordinate,omitempty"`
}

type PushParams struct {
	Screen  string          `json:"screen"`
	Payload json.RawMessage `json:"payload"`
	// Bundle is the app; absent reads it from the payload's Simulator Target Bundle.
	Bundle string `json:"bundle,omitempty"`
}

type ScreenshotResult struct {
	Image []byte `json:"image"` // JPEG
}

type SwitchSpaceParams struct {
	Screen string `json:"screen"`
	Space  string `json:"space"`
}

type AppParams struct {
	Screen string `json:"screen"`
	Bundle string `json:"bundle"`
}

type FocusWindowParams struct {
	Screen string `json:"screen"`
	Window int    `json:"window"`
}

type PressWindowButtonParams struct {
	Screen string            `json:"screen"`
	Button core.WindowButton `json:"button"`
}

type TileWindowParams struct {
	Screen string    `json:"screen"`
	Tile   core.Tile `json:"tile"`
}

type LaunchpadResult struct {
	Apps []core.App `json:"apps" wire:"array"`
}

type IconsParams struct {
	Screen  string   `json:"screen"`
	Bundles []string `json:"bundles" wire:"array"`
}

type IconsResult struct {
	Icons []core.Icon `json:"icons" wire:"array"`
}

// TapParams aims at one of ref (from screens.elements), label, or x and y.
type TapParams struct {
	Screen string `json:"screen"`
	core.Target
}

type SwipeParams struct {
	Screen string  `json:"screen"`
	FromX  float64 `json:"from_x"`
	FromY  float64 `json:"from_y"`
	ToX    float64 `json:"to_x"`
	ToY    float64 `json:"to_y"`
	// MS is how long the swipe takes; 300 when absent.
	MS int `json:"ms,omitempty"`
}

type TypeParams struct {
	Screen string `json:"screen"`
	Text   string `json:"text"`
}

type KeyParams struct {
	Screen string `json:"screen"`
	Key    string `json:"key"`
}

type ButtonParams struct {
	Screen string              `json:"screen"`
	Button core.HardwareButton `json:"button"`
}

type LaunchParams struct {
	Screen string `json:"screen"`
	// App is a bundle ID (iOS) or package (Android).
	App string `json:"app"`
}

type SavedScreenshot struct {
	Path string `json:"path"`
}

type SavedRecording struct {
	Path string `json:"path"`
}

// The tools' shared words: what screen to name, and the units.
const (
	toolScreen = " `screen` is an id from screens_list (a booted simulator or emulator)."
	toolUnits  = " Points on iOS, pixels on Android, as screens_elements reports them."
)

func init() {
	rpc.WireIn(core.StreamRequest{}, core.StreamInput{})
	rpc.WireOut(core.StreamMessage{}, core.ErrorData{})
}

func Register(r *rpc.Router, d Deps) {
	// list is every simulator and emulator here, iOS first; a platform
	// without its tools lists none.
	rpc.Add(r, "screens.list", list, rpc.MCP, rpc.Tool("This environment's iOS simulators and Android emulators, and its "+
		"display: each id, name, platform, OS, form and whether it's booted. Booted ones take the other screens_ tools."),
		rpc.Read)
	// boot answers once the device is ready; an Android cold boot takes up to a minute.
	rpc.Add(r, "screens.boot", d.boot, rpc.Detached, rpc.MCP, rpc.Tool("Boot a simulator or emulator and wait until it's "+
		"ready (an Android cold boot takes up to a minute)."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.shutdown", d.shutdown, rpc.MCP, rpc.Tool("Shut a simulator or emulator down."+toolScreen), rpc.Write)
	// view joins the screen's room and answers its view and desktop;
	// screens.view_changed and screens.desktop_changed follow.
	rpc.Add(r, "screens.view", d.view)
	rpc.Add(r, "screens.leave", d.leave)
	// set changes the settings given and answers the state after.
	rpc.Add(r, "screens.set", d.set, rpc.MCP, rpc.Tool("Change a simulator's or emulator's settings, only those given: "+
		"appearance (light, dark), content_size, contrast, status_bar, slow_animations, onscreen_keyboard, and pose "+
		"(closed, open, book, laptop) to fold a foldable. Answers the settings after."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.rotate", d.rotate, rpc.MCP, rpc.Tool("Turn a simulator or emulator a quarter, turn left or right."+
		toolScreen), rpc.Write)
	rpc.Add(r, "screens.open_url", d.openURL, rpc.MCP, rpc.Tool("Open a link on a simulator or emulator, in whichever app "+
		"takes it: a web page, or an app's own URL scheme to open the app."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.location", d.location)
	rpc.Add(r, "screens.push", d.push)
	rpc.Add(r, "screens.shake", d.shake)
	rpc.Add(r, "screens.memory_warning", d.memoryWarning)
	rpc.Add(r, "screens.screenshot", d.screenshot)
	// Driving a simulator or emulator by what's on its screen, for agents and `repogo sim`.
	rpc.Add(r, "screens.elements", d.elements, rpc.MCP, rpc.Tool("What's on a simulator's or emulator's screen now: each "+
		"element's ref, role, label, value, id and frame (x, y, w, h), and the screen's size."+toolUnits+
		" Refs (e7.12) name elements for screens_tap until the next read, fold or rotation. Read again after acting: "+
		"actions don't wait for the app to redraw."+toolScreen), rpc.Read)
	rpc.Add(r, "screens.tap", d.tap, rpc.MCP, rpc.Tool("Tap a simulator or emulator: one of ref (from screens_elements), "+
		"label (exact label, then exact id, then the one label containing it; reads the screen first), or x and y."+
		toolUnits+" Answers where it tapped."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.swipe", d.swipe, rpc.MCP, rpc.Tool("Drag one finger across a simulator or emulator, from from_x, "+
		"from_y to to_x, to_y over ms (300 by default): scroll, swipe back, pull to refresh."+toolUnits+toolScreen),
		rpc.Write)
	rpc.Add(r, "screens.type", d.typeText, rpc.MCP, rpc.Tool("Type text into the field that has focus on a simulator or "+
		"emulator: tap the field first."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.key", d.key, rpc.MCP, rpc.Tool("Press a key on a simulator or emulator: return, delete, "+
		"forward_delete, tab, escape, space, left, up, right or down."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.button", d.button, rpc.MCP, rpc.Tool("Press a hardware button on a simulator or emulator: home, "+
		"back, recents, app_switcher, lock, siri, volume_up or volume_down, as the screen takes them (an error lists "+
		"which)."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.launch", d.launch, rpc.MCP, rpc.Tool("Open an app on a simulator or emulator, or bring it to the "+
		"front, by its bundle ID (iOS, app.repogo) or package (Android, com.example.app)."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.recording.start", d.recordStart, rpc.MCP, rpc.Tool("Start recording a simulator's or emulator's "+
		"screen to a video; screens_recording_stop ends it. One recording per screen; one left running stops after "+
		"10 minutes."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.recording.stop", d.recordStop, rpc.MCP, rpc.Tool("Stop recording a simulator's or emulator's screen "+
		"and answer the video's path: an MP4 on iOS, a WebM on Android."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.save_screenshot", d.saveScreenshot, rpc.MCP, rpc.Tool("Save a simulator's or emulator's screen as a "+
		"PNG and answer its path, to open and look at."+toolScreen), rpc.Write)
	// The Mac's desktop. Moves need its live view open; launchpad and icons don't.
	rpc.Add(r, "screens.desktop.switch_space", d.switchSpace)
	rpc.Add(r, "screens.desktop.open_app", d.openApp)
	rpc.Add(r, "screens.desktop.quit_app", d.quitApp)
	rpc.Add(r, "screens.desktop.focus_window", d.focusWindow)
	rpc.Add(r, "screens.desktop.press_window_button", d.pressWindowButton)
	rpc.Add(r, "screens.desktop.tile_window", d.tileWindow)
	rpc.Add(r, "screens.desktop.launchpad", d.launchpad)
	rpc.Add(r, "screens.desktop.icons", d.icons)
}

func list(ctx context.Context, _ rpc.Caller, _ rpc.None) (ListResult, error) {
	screens, err := core.List(ctx)
	if screens == nil {
		screens = []core.Screen{}
	}
	// One platform's failure still lists the other's.
	if len(screens) > 0 {
		err = nil
	}
	return ListResult{Screens: screens}, err
}

func (d Deps) boot(ctx context.Context, _ rpc.Caller, p ScreenParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.Boot(ctx, p.Screen)
}

func (d Deps) shutdown(ctx context.Context, _ rpc.Caller, p ScreenParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.Shutdown(ctx, p.Screen)
}

func (d Deps) view(ctx context.Context, c rpc.Caller, p ScreenParams) (core.ViewResult, error) {
	return d.Screens.View(ctx, c.Device, p.Screen)
}

func (d Deps) leave(_ context.Context, c rpc.Caller, p ScreenParams) (rpc.Ack, error) {
	d.Screens.Leave(c.Device, p.Screen)
	return rpc.OK, nil
}

func (d Deps) set(ctx context.Context, _ rpc.Caller, p SetParams) (core.State, error) {
	return d.Screens.Set(ctx, p.Screen, p.Settings)
}

func (d Deps) rotate(ctx context.Context, _ rpc.Caller, p RotateParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.Rotate(ctx, p.Screen, p.Turn)
}

func (d Deps) openURL(ctx context.Context, _ rpc.Caller, p OpenURLParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.OpenURL(ctx, p.Screen, p.URL)
}

func (d Deps) location(ctx context.Context, _ rpc.Caller, p LocationParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.Location(ctx, p.Screen, p.Coordinate)
}

func (d Deps) push(ctx context.Context, _ rpc.Caller, p PushParams) (rpc.Ack, error) {
	if len(p.Payload) == 0 {
		return rpc.Ack{}, rpc.ErrInvalidParams
	}
	return rpc.OK, d.Screens.Push(ctx, p.Screen, p.Payload, p.Bundle)
}

func (d Deps) shake(ctx context.Context, _ rpc.Caller, p ScreenParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.Shake(ctx, p.Screen)
}

func (d Deps) memoryWarning(ctx context.Context, _ rpc.Caller, p ScreenParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.MemoryWarning(ctx, p.Screen)
}

func (d Deps) screenshot(ctx context.Context, _ rpc.Caller, p ScreenParams) (ScreenshotResult, error) {
	image, err := d.Screens.Screenshot(ctx, p.Screen)
	return ScreenshotResult{Image: image}, err
}

func (d Deps) switchSpace(ctx context.Context, _ rpc.Caller, p SwitchSpaceParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.DesktopSpace(ctx, p.Screen, p.Space)
}

func (d Deps) openApp(ctx context.Context, _ rpc.Caller, p AppParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.DesktopOpenApp(ctx, p.Screen, p.Bundle)
}

func (d Deps) quitApp(ctx context.Context, _ rpc.Caller, p AppParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.DesktopQuitApp(ctx, p.Screen, p.Bundle)
}

func (d Deps) focusWindow(ctx context.Context, _ rpc.Caller, p FocusWindowParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.DesktopWindow(ctx, p.Screen, p.Window)
}

func (d Deps) pressWindowButton(ctx context.Context, _ rpc.Caller, p PressWindowButtonParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.DesktopButton(ctx, p.Screen, p.Button)
}

func (d Deps) tileWindow(ctx context.Context, _ rpc.Caller, p TileWindowParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.DesktopTile(ctx, p.Screen, p.Tile)
}

func (d Deps) launchpad(ctx context.Context, _ rpc.Caller, p ScreenParams) (LaunchpadResult, error) {
	apps, err := d.Screens.Launchpad(ctx, p.Screen)
	return LaunchpadResult{Apps: apps}, err
}

func (d Deps) icons(ctx context.Context, _ rpc.Caller, p IconsParams) (IconsResult, error) {
	icons, err := d.Screens.Icons(ctx, p.Screen, p.Bundles)
	return IconsResult{Icons: icons}, err
}

// Stream serves a /stream socket: request is its first message, later
// messages are the phone's input; the screen goes out as frames. A message
// that isn't a StreamInput is skipped.
func (d Deps) Stream(ctx context.Context, caller rpc.Caller, request []byte, send func([]byte) error, reply func(any) error,
	input <-chan []byte) error {
	var req core.StreamRequest
	if err := json.Unmarshal(request, &req); err != nil || req.Screen == "" {
		return rpc.ErrInvalidParams
	}
	parsed := make(chan core.StreamInput)
	go func() {
		defer close(parsed)
		for b := range input {
			var in core.StreamInput
			if json.Unmarshal(b, &in) != nil {
				continue
			}
			select {
			case parsed <- in:
			case <-ctx.Done():
				return
			}
		}
	}()
	phone := core.Phone{ID: string(caller.Device)}
	if d.PhoneName != nil {
		phone.Name = d.PhoneName(caller.Device)
	}
	return d.Screens.Stream(ctx, req, phone, send, reply, parsed)
}

func (d Deps) elements(ctx context.Context, _ rpc.Caller, p ScreenParams) (core.Elements, error) {
	return d.Screens.Elements(ctx, p.Screen)
}

func (d Deps) tap(ctx context.Context, _ rpc.Caller, p TapParams) (core.Point, error) {
	return d.Screens.Tap(ctx, p.Screen, p.Target)
}

func (d Deps) swipe(ctx context.Context, _ rpc.Caller, p SwipeParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.Swipe(ctx, p.Screen, core.Point{X: p.FromX, Y: p.FromY}, core.Point{X: p.ToX, Y: p.ToY}, p.MS)
}

func (d Deps) typeText(ctx context.Context, _ rpc.Caller, p TypeParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.Type(ctx, p.Screen, p.Text)
}

func (d Deps) key(ctx context.Context, _ rpc.Caller, p KeyParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.Key(ctx, p.Screen, p.Key)
}

func (d Deps) button(ctx context.Context, _ rpc.Caller, p ButtonParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.Button(ctx, p.Screen, p.Button)
}

func (d Deps) saveScreenshot(ctx context.Context, _ rpc.Caller, p ScreenParams) (SavedScreenshot, error) {
	path, err := d.Screens.SaveScreenshot(ctx, p.Screen)
	return SavedScreenshot{Path: path}, err
}

func (d Deps) launch(ctx context.Context, _ rpc.Caller, p LaunchParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.Launch(ctx, p.Screen, p.App)
}

func (d Deps) recordStart(ctx context.Context, _ rpc.Caller, p ScreenParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.RecordStart(ctx, p.Screen)
}

func (d Deps) recordStop(ctx context.Context, _ rpc.Caller, p ScreenParams) (SavedRecording, error) {
	path, err := d.Screens.RecordStop(ctx, p.Screen)
	return SavedRecording{Path: path}, err
}
