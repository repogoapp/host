package screens

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/repogoapp/host/internal/screens/capture"
)

// ListIOS reads `xcrun simctl list devices available -j`.
func ListIOS(ctx context.Context) ([]Screen, error) {
	if ios, _ := Available(); !ios {
		return nil, nil
	}
	out, err := run(ctx, "xcrun", "simctl", "list", "devices", "available", "-j")
	if err != nil {
		return nil, err
	}
	return parseSimctl(out)
}

func parseSimctl(out []byte) ([]Screen, error) {
	var list struct {
		Devices map[string][]struct {
			UDID     string `json:"udid"`
			Name     string `json:"name"`
			State    string `json:"state"`
			DataPath string `json:"dataPath"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	var screens []Screen
	for runtimeID, sims := range list.Devices {
		os := runtimeOS(runtimeID)
		// watchOS, tvOS and visionOS simulators aren't screens the phone drives.
		if os.Name != "iOS" {
			continue
		}
		for _, s := range sims {
			screens = append(screens, Screen{
				Kind:     KindSimulator,
				Platform: PlatformIOS,
				ID:       s.UDID,
				Name:     s.Name,
				OS:       os,
				State:    simState(s.State),
				Form:     iosForm(s.Name),
				data:     s.DataPath,
			})
		}
	}
	sortDevices(screens)
	return screens, nil
}

// runtimeOS reads "com.apple.CoreSimulator.SimRuntime.iOS-27-1" as iOS 27.1.
func runtimeOS(id string) OS {
	name := id[strings.LastIndex(id, ".")+1:]
	platform, version, ok := strings.Cut(name, "-")
	if !ok {
		return OS{Name: name}
	}
	return OS{Name: platform, Version: strings.ReplaceAll(version, "-", ".")}
}

func simState(s string) ScreenState {
	switch s {
	case "Booted":
		return StateBooted
	case "Booting":
		return StateBooting
	case "Shutting Down":
		return StateShuttingDown
	}
	return StateShutdown
}

// iosForm is a simulator's shape from its name: iPads are tablets, and
// the foldable iPhones say so in theirs.
func iosForm(name string) Form {
	switch {
	case strings.HasPrefix(name, "iPad"):
		return FormTablet
	case strings.Contains(name, "Duo") || strings.Contains(name, "Fold"):
		return FormFoldable
	}
	return FormPhone
}

// streamIOS runs `repogo simcap` for the device: this binary again, so the
// private SimulatorKit it loads can crash only this view.
func streamIOS(ctx context.Context, c *controller, framed *framer, input *inputQueue) error {
	args := []string{"simcap", c.d.ID}
	if c.universalHID() {
		args = append(args, "universal")
	}
	return streamChild(ctx, c, framed.child, framed.pace, input, args, nil)
}

// universalHID reports whether the device takes touches and keys through
// Universal HID: iOS 27 ignores the Indigo HID messages older runtimes take.
func (c *controller) universalHID() bool {
	return c.d.Platform == PlatformIOS && iosMajor(c.d.OS.Version) >= 27
}

func (c *controller) readIOS(ctx context.Context) State {
	ui := func(option string) string {
		out, _ := c.simctl(ctx, "ui", c.d.ID, option)
		return strings.TrimSpace(string(out))
	}
	var s State
	if v := Appearance(ui("appearance")); v == AppearanceLight || v == AppearanceDark {
		s.Appearance = &v
	}
	if v := ContentSize(ui("content_size")); v.valid() {
		s.ContentSize = &v
	}
	if v := ui("increase_contrast"); v != "" {
		s.Contrast = ptr(v == "enabled")
	}
	return s
}

func (c *controller) simctl(ctx context.Context, args ...string) ([]byte, error) {
	return run(ctx, "xcrun", append([]string{"simctl"}, args...)...)
}

func (c *controller) setIOS(ctx context.Context, s Settings) error {
	id := c.d.ID
	if v := s.Appearance; v != nil {
		if _, err := c.simctl(ctx, "ui", id, "appearance", string(*v)); err != nil {
			return err
		}
		c.update(func(st *State) { st.Appearance = ptr(*v) })
	}
	if v := s.ContentSize; v != nil {
		if _, err := c.simctl(ctx, "ui", id, "content_size", string(*v)); err != nil {
			return err
		}
		c.update(func(st *State) { st.ContentSize = ptr(*v) })
	}
	if v := s.Contrast; v != nil {
		if _, err := c.simctl(ctx, "ui", id, "increase_contrast", map[bool]string{true: "enabled", false: "disabled"}[*v]); err != nil {
			return err
		}
		c.update(func(st *State) { st.Contrast = ptr(*v) })
	}
	if v := s.StatusBar; v != nil {
		args := []string{"status_bar", id, "clear"}
		if *v {
			args = []string{"status_bar", id, "override", "--time", "9:41", "--dataNetwork", "wifi",
				"--wifiMode", "active", "--wifiBars", "3", "--cellularMode", "active", "--cellularBars", "4",
				"--batteryState", "charged", "--batteryLevel", "100", "--operatorName", ""}
		}
		if _, err := c.simctl(ctx, args...); err != nil {
			return err
		}
		c.update(func(st *State) { st.StatusBar = ptr(*v) })
	}
	if v := s.SlowAnimations; v != nil {
		if err := c.command(ctx, capture.Input{Action: capture.ActionSlowAnimations, Value: onOff(*v)}); err != nil {
			return err
		}
		c.update(func(st *State) { st.SlowAnimations = ptr(*v) })
	}
	if v := s.OnscreenKeyboard; v != nil {
		if err := c.command(ctx, capture.Input{Action: capture.ActionKeyboard, Value: onOff(*v)}); err != nil {
			return err
		}
		c.update(func(st *State) { st.OnscreenKeyboard = ptr(*v) })
	}
	if v := s.Pose; v != nil {
		if err := c.command(ctx, capture.Input{Action: capture.ActionFold, Value: string(*v)}); err != nil {
			return err
		}
		c.update(func(st *State) { st.Pose = ptr(*v) })
	}
	return nil
}

func (c *controller) rotateIOS(ctx context.Context, t Turn) error {
	c.mu.Lock()
	foldable := c.foldable || c.d.Form == FormFoldable
	c.mu.Unlock()
	// Xcode's own command where it turns the device: it reads the real
	// orientation first, so a turn made outside the app is counted. A
	// foldable takes it and stays put, and an older Xcode hasn't got it.
	if !foldable {
		if now, ok := deviceOrientation(ctx, c.d.ID); ok {
			next := turn(now, t)
			c.aimAt(next)
			_, err := run(ctx, "xcrun", "devicectl", "device", "orientation", "set", "--device", c.d.ID,
				orientationNames[next])
			if err == nil {
				c.turnedTo(next)
				return nil
			}
			c.missed()
		}
	}
	// Else through simcap: Device Hub's control on iOS 27, and before it
	// Simulator.app's own event, which needs Simulator.app running.
	if !c.universalHID() && exec.Command("pgrep", "-xq", "Simulator").Run() != nil {
		return errors.New("Open Simulator on the Mac to rotate.")
	}
	c.mu.Lock()
	next := turn(c.orientation, t)
	c.mu.Unlock()
	c.aimAt(next)
	if err := c.command(ctx, capture.Input{Action: capture.ActionRotate, Value: strconv.Itoa(next)}); err != nil {
		c.missed()
		return err
	}
	c.turnedTo(next)
	return nil
}

// aimAt says where the video will turn to, taken by the first frame of the
// turn; a display simcap reports says so too, later, and sets it right if not.
func (c *controller) aimAt(orientation int) {
	c.mu.Lock()
	c.pendingTurn = ptr(iosRotation(orientation))
	c.mu.Unlock()
}

// missed forgets the turn aimed at: the rotate failed.
func (c *controller) missed() {
	c.mu.Lock()
	c.pendingTurn = nil
	c.mu.Unlock()
}

// turnedTo records the device's new orientation and has the phone shown it.
func (c *controller) turnedTo(orientation int) {
	c.mu.Lock()
	c.orientation = orientation
	// No frame took it: the screen is still (a Home screen that doesn't turn).
	if c.pendingTurn != nil {
		c.rotation, c.pendingTurn = *c.pendingTurn, nil
	}
	c.mu.Unlock()
	c.showTurn()
}

// orientationNames are the UIDeviceOrientations as devicectl names them.
var orientationNames = map[int]string{1: "portrait", 2: "portraitUpsideDown", 3: "landscapeLeft", 4: "landscapeRight"}

// deviceOrientation asks devicectl how the device is held; false when this
// Xcode can't say, or it lies flat.
func deviceOrientation(ctx context.Context, id string) (int, bool) {
	out, err := run(ctx, "xcrun", "devicectl", "device", "orientation", "get", "--device", id)
	if err != nil {
		return 0, false
	}
	return parseOrientation(string(out))
}

// parseOrientation reads devicectl's "Current Device Orientation: landscapeLeft".
func parseOrientation(out string) (int, bool) {
	_, name, found := strings.Cut(out, "Device Orientation:")
	if !found {
		return 0, false
	}
	name, _, _ = strings.Cut(strings.TrimSpace(name), "\n")
	for orientation, known := range orientationNames {
		if known == strings.TrimSpace(name) {
			return orientation, true
		}
	}
	return 0, false
}

func (c *controller) locationIOS(ctx context.Context, at *Coordinate) error {
	if at == nil {
		_, err := c.simctl(ctx, "location", c.d.ID, "clear")
		return err
	}
	_, err := c.simctl(ctx, "location", c.d.ID, "set", fmt.Sprintf("%f,%f", at.Lat, at.Lon))
	return err
}

func (c *controller) pushIOS(ctx context.Context, payload []byte, bundle string) error {
	args := []string{"simctl", "push", c.d.ID}
	if bundle != "" {
		args = append(args, bundle)
	}
	_, err := runInput(ctx, string(payload), "xcrun", append(args, "-")...)
	return err
}

func (c *controller) screenshotIOS(ctx context.Context) ([]byte, error) {
	// simctl takes "-" for stdout but writes a file named "-", so it
	// writes a temporary file instead.
	dir, err := os.MkdirTemp("", "simshot")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "shot.jpg")
	// The panel the stream shows: left to simctl, a foldable's shot is
	// its outer screen, black while it's open.
	args := []string{"io", c.d.ID, "screenshot", "--type=jpeg"}
	c.mu.Lock()
	if c.activeScreen != 0 {
		args = append(args, "--display="+strconv.FormatUint(uint64(c.activeScreen), 10))
	}
	c.mu.Unlock()
	if _, err := c.simctl(ctx, append(args, path)...); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// typeIOS types text in order: runs a US keyboard has keys for as keys,
// the rest (an emoji, an accent) through the pasteboard and ⌘V.
func (c *controller) typeIOS(ctx context.Context, text string, device func(capture.Input) error) error {
	for _, run := range splitTypeable(text) {
		if run.keys {
			if err := device(capture.Input{Action: capture.ActionText, Value: run.text}); err != nil {
				return err
			}
			continue
		}
		if _, err := runInput(ctx, run.text, "xcrun", "simctl", "pbcopy", c.d.ID); err != nil {
			continue
		}
		if err := device(capture.Input{Action: capture.ActionPasteKeys}); err != nil {
			return err
		}
	}
	return nil
}

type textRun struct {
	text string
	keys bool
}

func splitTypeable(text string) []textRun {
	var runs []textRun
	for _, r := range text {
		keys := capture.Typeable(r)
		if n := len(runs); n > 0 && runs[n-1].keys == keys {
			runs[n-1].text += string(r)
			continue
		}
		runs = append(runs, textRun{string(r), keys})
	}
	return runs
}

// upright is how far, clockwise, the phone turns a framebuffer whose screen
// CoreDevice reports at "rot<degrees>" to show it upright: the same amount
// (checked on an iPhone Duo's inner panel, rot90), as -180 to 180.
func upright(degrees int) int {
	switch ((degrees % 360) + 360) % 360 {
	case 90:
		return 90
	case 180:
		return 180
	case 270:
		return -90
	}
	return 0
}

// iosRotation is how far the phone turns simcap's video, which stays in the
// device's portrait framebuffer while the screen inside it turns.
func iosRotation(orientation int) int {
	switch orientation {
	case 2:
		return 180
	case 3:
		return -90
	case 4:
		return 90
	}
	return 0
}

// orientationAt is the UIDeviceOrientation of a device whose display simcap
// reports turned by degrees: what a rotate to it reports back.
func orientationAt(degrees int) int {
	switch ((degrees % 360) + 360) % 360 {
	case 90:
		return 4
	case 180:
		return 2
	case 270:
		return 3
	}
	return 1
}

// turn is the orientation after rotating the device left or right a quarter.
func turn(orientation int, dir Turn) int {
	// Turning left goes portrait, landscape left, upside down, landscape right.
	order := []int{1, 3, 2, 4}
	i := max(slices.Index(order, orientation), 0)
	if dir == TurnRight {
		return order[(i+3)%4]
	}
	return order[(i+1)%4]
}

func iosMajor(version string) int {
	major, _, _ := strings.Cut(version, ".")
	n, _ := strconv.Atoi(major)
	return n
}
