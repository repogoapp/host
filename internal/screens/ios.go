package screens

import (
	"context"
	"encoding/base64"
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
			UDID  string `json:"udid"`
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, err
	}
	var screens []Screen
	for runtimeID, sims := range list.Devices {
		for _, s := range sims {
			screens = append(screens, Screen{
				Kind:     KindSimulator,
				Platform: "ios",
				ID:       s.UDID,
				Name:     s.Name,
				OS:       runtimeName(runtimeID),
				Booted:   s.State == "Booted",
			})
		}
	}
	sortDevices(screens)
	return screens, nil
}

// runtimeName turns "com.apple.CoreSimulator.SimRuntime.iOS-27-1" into "iOS 27.1".
func runtimeName(id string) string {
	name := id[strings.LastIndex(id, ".")+1:]
	platform, version, ok := strings.Cut(name, "-")
	if !ok {
		return name
	}
	return platform + " " + strings.ReplaceAll(version, "-", ".")
}

// streamIOS runs `repogo simcap` for the device: this binary again, so the
// private SimulatorKit it loads can crash only this view.
func streamIOS(ctx context.Context, c *controller, send func([]byte) error, input <-chan capture.Input) error {
	args := []string{"simcap", c.d.ID}
	if c.universalHID() {
		args = append(args, "universal")
	}
	return streamChild(ctx, c, send, input, args, nil)
}

// universalHID reports whether the device takes touches and keys through
// Universal HID: iOS 27 ignores the Indigo HID messages older runtimes take.
func (c *controller) universalHID() bool { return c.d.Platform == "ios" && iosMajor(c.d.OS) >= 27 }

func (c *controller) readIOS(ctx context.Context) {
	ui := func(option string) string {
		out, _ := c.simctl(ctx, "ui", c.d.ID, option)
		return strings.TrimSpace(string(out))
	}
	c.state.Appearance = ui("appearance")
	c.state.ContentSize = ui("content_size")
	c.state.Contrast = ui("increase_contrast") == "enabled"
}

func (c *controller) simctl(ctx context.Context, args ...string) ([]byte, error) {
	return run(ctx, "xcrun", append([]string{"simctl"}, args...)...)
}

func (c *controller) actIOS(ctx context.Context, in capture.Input, forward func(capture.Input) error) error {
	id := c.d.ID
	if in.Button == "home" {
		_, err := c.simctl(ctx, "launch", id, "com.apple.springboard")
		return err
	}
	var err error
	switch in.Action {
	case "appearance":
		if _, err = c.simctl(ctx, "ui", id, "appearance", lightOrDark(in.Value)); err == nil {
			c.set(func(s *State) { s.Appearance = lightOrDark(in.Value) })
		}
	case "content_size":
		size, ok := contentSize(in.Value)
		if !ok {
			return fmt.Errorf("unknown text size %q", in.Value)
		}
		if _, err = c.simctl(ctx, "ui", id, "content_size", size); err == nil {
			c.set(func(s *State) { s.ContentSize = size })
		}
	case "contrast":
		on := in.Value == "on"
		if _, err = c.simctl(ctx, "ui", id, "increase_contrast", map[bool]string{true: "enabled", false: "disabled"}[on]); err == nil {
			c.set(func(s *State) { s.Contrast = on })
		}
	case "rotate":
		// CoreDevice turns an iOS 27 device itself; older runtimes turn
		// through Simulator.app, which must be running.
		if !c.universalHID() && exec.Command("pgrep", "-xq", "Simulator").Run() != nil {
			return errors.New("open Simulator on the Mac to rotate")
		}
		c.mu.Lock()
		next := turn(c.state.Orientation, in.Value)
		displayed := c.displayed
		c.mu.Unlock()
		if err = forward(capture.Input{Action: capture.ActionRotate, Value: strconv.Itoa(next)}); err == nil {
			c.set(func(s *State) {
				s.Orientation = next
				if !displayed {
					s.Rotation = iosRotation(next)
				}
			})
		}
	case "fold":
		if err = forward(capture.Input{Action: capture.ActionFold, Value: in.Value}); err == nil {
			c.set(func(s *State) { s.Pose = in.Value })
		}
	case capture.ActionKeyboard:
		on := in.Value == "on"
		if err = forward(in); err == nil {
			c.set(func(s *State) { s.OnscreenKeyboard = on })
		}
	case "url":
		_, err = c.simctl(ctx, "openurl", id, in.Value)
	case "location":
		if in.Value == "clear" {
			_, err = c.simctl(ctx, "location", id, "clear")
		} else {
			_, err = c.simctl(ctx, "location", id, "set", fmt.Sprintf("%f,%f", in.Lat, in.Lon))
		}
	case "status_bar":
		on := in.Value == "on"
		if on {
			_, err = c.simctl(ctx, "status_bar", id, "override", "--time", "9:41", "--dataNetwork", "wifi",
				"--wifiMode", "active", "--wifiBars", "3", "--cellularMode", "active", "--cellularBars", "4",
				"--batteryState", "charged", "--batteryLevel", "100", "--operatorName", "")
		} else {
			_, err = c.simctl(ctx, "status_bar", id, "clear")
		}
		if err == nil {
			c.set(func(s *State) { s.StatusBar = on })
		}
	case capture.ActionSlowAnimations:
		on := in.Value == "on"
		if err = forward(in); err == nil {
			c.set(func(s *State) { s.SlowAnimations = on })
		}
	case "paste":
		// Onto the device's pasteboard, then ⌘V into the focused field: how
		// text a key can't make (an emoji, an accent) is typed.
		if _, err = runInput(ctx, in.Value, "xcrun", "simctl", "pbcopy", id); err == nil {
			err = forward(capture.Input{Action: capture.ActionPasteKeys})
		}
	case "push":
		args := []string{"simctl", "push", id}
		if in.Bundle != "" {
			args = append(args, in.Bundle)
		}
		_, err = runInput(ctx, in.Value, "xcrun", append(args, "-")...)
	case "shake":
		_, err = c.simctl(ctx, "spawn", id, "notifyutil", "-p", "com.apple.UIKit.SimulatorShake")
	case "screenshot":
		// simctl takes "-" for stdout but writes a file named "-", so it
		// writes a temporary file instead.
		dir, derr := os.MkdirTemp("", "simshot")
		if derr != nil {
			return derr
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "shot.jpg")
		// The panel the stream shows: left to simctl, a foldable's shot is
		// its outer screen, black while it's open.
		args := []string{"io", id, "screenshot", "--type=jpeg"}
		c.mu.Lock()
		if c.activeScreen != 0 {
			args = append(args, "--display="+strconv.FormatUint(uint64(c.activeScreen), 10))
		}
		c.mu.Unlock()
		if _, err = c.simctl(ctx, append(args, path)...); err == nil {
			var shot []byte
			if shot, err = os.ReadFile(path); err == nil {
				c.reply(Message{Screenshot: base64.StdEncoding.EncodeToString(shot)})
			}
		}
	default:
		return fmt.Errorf("%s isn't available on this simulator", in.Action)
	}
	return err
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

// turn is the orientation after rotating the device left or right a quarter.
func turn(orientation int, dir string) int {
	// Turning left goes portrait, landscape left, upside down, landscape right.
	order := []int{1, 3, 2, 4}
	i := max(slices.Index(order, orientation), 0)
	if dir == "right" {
		return order[(i+3)%4]
	}
	return order[(i+1)%4]
}

func iosMajor(os string) int {
	_, version, _ := strings.Cut(os, " ")
	major, _, _ := strings.Cut(version, ".")
	n, _ := strconv.Atoi(major)
	return n
}
