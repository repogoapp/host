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

func init() {
	rpc.WireIn(core.StreamRequest{}, core.StreamInput{})
	rpc.WireOut(core.StreamMessage{}, core.ErrorData{})
}

func Register(r *rpc.Router, d Deps) {
	// list is every simulator and emulator here, iOS first; a platform
	// without its tools lists none.
	rpc.Add(r, "screens.list", list)
	// boot answers once the device is ready; an Android cold boot takes up to a minute.
	rpc.Add(r, "screens.boot", d.boot, rpc.Detached)
	rpc.Add(r, "screens.shutdown", d.shutdown)
	// view joins the screen's room and answers its view and desktop;
	// screens.view_changed and screens.desktop_changed follow.
	rpc.Add(r, "screens.view", d.view)
	rpc.Add(r, "screens.leave", d.leave)
	// set changes the settings given and answers the state after.
	rpc.Add(r, "screens.set", d.set)
	rpc.Add(r, "screens.rotate", d.rotate)
	rpc.Add(r, "screens.open_url", d.openURL)
	rpc.Add(r, "screens.location", d.location)
	rpc.Add(r, "screens.push", d.push)
	rpc.Add(r, "screens.shake", d.shake)
	rpc.Add(r, "screens.memory_warning", d.memoryWarning)
	rpc.Add(r, "screens.screenshot", d.screenshot)
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
