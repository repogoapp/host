// Package screens is the `screens.*` methods, and the /stream request that
// shows one: this machine's simulators, emulators and its display. The tools
// are driven in internal/screens.
package screens

import (
	"context"
	"encoding/json"
	"strings"

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
	// Grant is permissions to grant it first (screens.GrantNames).
	Grant []string `json:"grant,omitempty"`
}

type FoldParams struct {
	Screen string    `json:"screen"`
	Pose   core.Pose `json:"pose"`
}

type WaitParams struct {
	Screen string `json:"screen"`
	core.WaitFor
}

// Tapped is where a tap landed and, to an agent, the screen once it settled.
type Tapped struct {
	X    float64        `json:"x"`
	Y    float64        `json:"y"`
	Read *core.Elements `json:"read,omitempty"`
}

// Acted is an action done and, to an agent, the screen once it settled: the
// read it would make next, done for it.
type Acted struct {
	OK   bool           `json:"ok"`
	Read *core.Elements `json:"read,omitempty"`
}

type SavedScreenshot struct {
	Path string `json:"path"`
	// Image is the picture itself, for an agent: its MCP answer shows it, so
	// one on any environment sees it without reading the file.
	Image *Image `json:"image,omitempty"`
}

// Image is a picture in an answer, as base64.
type Image struct {
	MimeType string `json:"mime_type"`
	Data     []byte `json:"data"`
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
		"(closed, open, book, laptop). Answers the settings after; to fold and see the screen after, use screens_fold."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.rotate", d.rotate, rpc.MCP, rpc.Tool("Turn a simulator or emulator a quarter, turn left or right."+
		toolScreen), rpc.Write)
	rpc.Add(r, "screens.open_url", d.openURL, rpc.MCP, rpc.Tool("Open a link on a simulator or emulator, in whichever app "+
		"takes it: a web page, or an app's own URL scheme to open the app."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.location", d.location)
	rpc.Add(r, "screens.push", d.push)
	rpc.Add(r, "screens.shake", d.shake)
	rpc.Add(r, "screens.memory_warning", d.memoryWarning)
	rpc.Add(r, "screens.screenshot", d.screenshot)
	// Driving a simulator or emulator by what's on its screen, for agents and `repogo screens`.
	rpc.Add(r, "screens.elements", d.elements, rpc.MCP, rpc.Tool("What's on a simulator's or emulator's screen now: each "+
		"element's ref, role, label, value, id and frame (x, y, w, h), and the screen's size."+toolUnits+
		" Refs (e7.12) name elements for screens_tap and last while the element stays on the screen. States "+
		"follow the label: selected, on/off (switches), disabled, focused, secure (a password field), covered "+
		"(under the keyboard: hide it first). keyboard says the on-screen keyboard is up; pose is a foldable's. "+
		"Actions answer the screen once it settles, as changes when most of it stayed: changes true, elements "+
		"lists only what's new or changed, gone the refs no longer there, and every other ref still holds. Read "+
		"only to start, or after something that answers none."+toolScreen), rpc.Read)
	rpc.Add(r, "screens.fold", d.fold, rpc.MCP, rpc.Tool("Fold a foldable simulator or emulator: pose closed, open, "+
		"book (half open) or laptop. Answers once the fold has shown, with the screen after it: its new size and "+
		"elements, with fresh refs."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.wait", d.wait, rpc.MCP, rpc.Tool("Wait on a simulator's or emulator's screen instead of "+
		"sleeping: until an element whose label, id or field text contains label shows, until one containing gone "+
		"is gone, or until the screen is stable (two reads alike), up to timeout_ms (10000 by default, at most "+
		"60000). Answers met, how long it waited, and the screen as last read, with current refs, so the next "+
		"action needs no read of its own. Use it after an action that takes a moment (opening a chat, a sheet, "+
		"a page loading)."+toolScreen), rpc.Read)
	rpc.Add(r, "screens.tap", d.tap, rpc.MCP, rpc.Tool("Tap a simulator or emulator: one of ref (from screens_elements), "+
		"label (exact label, then exact id, then the one label containing it; reads the screen first), or x and y."+
		toolUnits+" Refuses an element off the screen or under the keyboard. Answers where it tapped and the screen "+
		"once it settled, as changes where most of it stayed: no read needed after."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.swipe", d.swipe, rpc.MCP, rpc.Tool("Drag one finger across a simulator or emulator, from from_x, "+
		"from_y to to_x, to_y over ms (300 by default): scroll, swipe back, pull to refresh."+toolUnits+toolScreen),
		rpc.Write)
	rpc.Add(r, "screens.type", d.typeText, rpc.MCP, rpc.Tool("Type text into the field that has focus on a simulator or "+
		"emulator: tap the field first."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.key", d.key, rpc.MCP, rpc.Tool("Press a key on a simulator or emulator: "+
		strings.Join(core.KeyNames(), ", ")+"."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.button", d.button, rpc.MCP, rpc.Tool("Press a hardware button on a simulator or emulator: home, "+
		"back, recents, app_switcher, lock, siri, volume_up or volume_down, as the screen takes them (an error lists "+
		"which)."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.launch", d.launch, rpc.MCP, rpc.Tool("Open an app on a simulator or emulator, or bring it to the "+
		"front, by its bundle ID (iOS, app.repogo) or package (Android, com.example.app). grant lists permissions to "+
		"grant it first, so it asks for none: "+strings.Join(core.GrantNames(), ", ")+" (iOS has no camera or "+
		"notifications grant). Granting can quit an app that's running."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.recording.start", d.recordStart, rpc.MCP, rpc.Tool("Start recording a simulator's or emulator's "+
		"screen to a video; screens_recording_stop ends it. One recording per screen; one left running stops after "+
		"10 minutes (3 on an Android phone)."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.recording.stop", d.recordStop, rpc.MCP, rpc.Tool("Stop recording a simulator's or emulator's screen "+
		"and answer the video's path: an MP4 on iOS and Android phones, a WebM on Android emulators."+toolScreen), rpc.Write)
	rpc.Add(r, "screens.save_screenshot", d.saveScreenshot, rpc.MCP, rpc.Tool("Save a simulator's or emulator's screen as a "+
		"PNG and answer its path; to an agent it also answers the picture itself, to look at. Password fields "+
		"are blacked out."+toolScreen), rpc.Write)
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

func (d Deps) elements(ctx context.Context, _ rpc.Caller, p ScreenParams) (core.Elements, error) {
	return d.Screens.Elements(ctx, p.Screen)
}

func (d Deps) fold(ctx context.Context, c rpc.Caller, p FoldParams) (Acted, error) {
	_, err := d.Screens.Set(ctx, p.Screen, core.Settings{Pose: &p.Pose})
	return d.acted(ctx, c, p.Screen, err)
}

func (d Deps) wait(ctx context.Context, _ rpc.Caller, p WaitParams) (core.Waited, error) {
	return d.Screens.Wait(ctx, p.Screen, p.WaitFor)
}

func (d Deps) tap(ctx context.Context, c rpc.Caller, p TapParams) (Tapped, error) {
	at, err := d.Screens.Tap(ctx, p.Screen, p.Target)
	if err != nil {
		return Tapped{}, err
	}
	return Tapped{X: at.X, Y: at.Y, Read: d.settled(ctx, c, p.Screen)}, nil
}

func (d Deps) swipe(ctx context.Context, c rpc.Caller, p SwipeParams) (Acted, error) {
	err := d.Screens.Swipe(ctx, p.Screen, core.Point{X: p.FromX, Y: p.FromY}, core.Point{X: p.ToX, Y: p.ToY}, p.MS)
	return d.acted(ctx, c, p.Screen, err)
}

func (d Deps) typeText(ctx context.Context, c rpc.Caller, p TypeParams) (Acted, error) {
	return d.acted(ctx, c, p.Screen, d.Screens.Type(ctx, p.Screen, p.Text))
}

func (d Deps) key(ctx context.Context, c rpc.Caller, p KeyParams) (Acted, error) {
	return d.acted(ctx, c, p.Screen, d.Screens.Key(ctx, p.Screen, p.Key))
}

func (d Deps) button(ctx context.Context, c rpc.Caller, p ButtonParams) (Acted, error) {
	return d.acted(ctx, c, p.Screen, d.Screens.Button(ctx, p.Screen, p.Button))
}

func (d Deps) launch(ctx context.Context, c rpc.Caller, p LaunchParams) (Acted, error) {
	return d.acted(ctx, c, p.Screen, d.Screens.Launch(ctx, p.Screen, p.App, p.Grant))
}

func (d Deps) recordStart(ctx context.Context, _ rpc.Caller, p ScreenParams) (rpc.Ack, error) {
	return rpc.OK, d.Screens.RecordStart(ctx, p.Screen)
}

func (d Deps) recordStop(ctx context.Context, _ rpc.Caller, p ScreenParams) (SavedRecording, error) {
	path, err := d.Screens.RecordStop(ctx, p.Screen)
	return SavedRecording{Path: path}, err
}

func (d Deps) saveScreenshot(ctx context.Context, c rpc.Caller, p ScreenParams) (SavedScreenshot, error) {
	path, err := d.Screens.SaveScreenshot(ctx, p.Screen)
	if err != nil || c.Agent == nil {
		return SavedScreenshot{Path: path}, err
	}
	shot, err := core.ScreenshotForAgent(path)
	if err != nil {
		return SavedScreenshot{Path: path}, nil
	}
	return SavedScreenshot{Path: path, Image: &Image{MimeType: "image/jpeg", Data: shot}}, nil
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

// settled is the screen after an action, for an agent; nil for anyone else.
func (d Deps) settled(ctx context.Context, c rpc.Caller, screen string) *core.Elements {
	if c.Agent == nil {
		return nil
	}
	return d.Screens.Settled(ctx, screen)
}

// acted answers an action, with the screen after it for an agent.
func (d Deps) acted(ctx context.Context, c rpc.Caller, screen string, err error) (Acted, error) {
	if err != nil {
		return Acted{}, err
	}
	return Acted{OK: true, Read: d.settled(ctx, c, screen)}, nil
}
