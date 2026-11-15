// Package screens is the live screens on this machine a phone can view and
// control: its iOS simulators, Android emulators and its own display. Each
// source lists, starts and streams in its own file (ios.go, android.go,
// display.go); a source whose tools are missing lists nothing, not an error.
package screens

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/proctree"
)

// Screen is one live screen: a simulator, an emulator or this machine's display.
type Screen struct {
	// Kind is what it is (simulator, emulator or display); Platform its OS.
	Kind     string `json:"kind"`
	Platform string `json:"platform"` // "ios", "android" or "macos"
	ID       string `json:"id"`       // simulator UDID, AVD name, or "display"
	Name     string `json:"name"`
	OS       string `json:"os"` // "iOS 27.1"; the AVD's API level on Android
	Booted   bool   `json:"booted"`
	Serial   string `json:"serial,omitempty"` // adb serial (emulator-5554) while an emulator runs
	// User is the display's account the host signs in as, for the phone to name.
	User string `json:"user,omitempty"`
}

// The kinds of screen.
const (
	KindSimulator = "simulator"
	KindEmulator  = "emulator"
	KindDisplay   = "display"
)

// List returns every available device, iOS first, each platform sorted by
// name, then this Mac while Screen Sharing is on. An error from one platform
// is returned alongside the other platforms' screens.
func List(ctx context.Context) ([]Screen, error) {
	ios, iosErr := ListIOS(ctx)
	android, androidErr := ListAndroid(ctx)
	screens := append(ios, android...)
	if mac, ok := ThisDisplay(ctx); ok {
		screens = append(screens, mac)
	}
	return screens, errors.Join(iosErr, androidErr)
}

func sortDevices(screens []Screen) {
	sort.SliceStable(screens, func(i, j int) bool {
		if screens[i].Name != screens[j].Name {
			return screens[i].Name < screens[j].Name
		}
		return screens[i].OS > screens[j].OS
	})
}

// find looks id up among this machine's screens, so only an id the tools
// listed ever reaches a command.
func find(ctx context.Context, id string) (Screen, error) {
	screens, err := List(ctx)
	for _, d := range screens {
		if d.ID == id {
			return d, nil
		}
	}
	if err != nil {
		return Screen{}, err
	}
	return Screen{}, ErrUnknown
}

// Supported reports whether this machine can have live screens at all: a
// Mac shows its own display, and any machine with Xcode's or the Android
// SDK's tools has simulators or emulators. screens.list says which are there.
func Supported() bool {
	ios, android := Available()
	return runtime.GOOS == "darwin" || ios || android
}

// Available reports which platforms' tools this machine has, read once: a
// tool installed later shows after the host restarts.
var Available = sync.OnceValues(func() (ios, android bool) {
	if runtime.GOOS == "darwin" {
		// xcrun is on every Mac; simctl only where Xcode is installed.
		ios = exec.Command("xcrun", "--find", "simctl").Run() == nil
	}
	return ios, androidTool("emulator", "emulator") != ""
})

// DeveloperDir is the Xcode `xcode-select` points at, whose SimulatorKit
// simcap loads.
func DeveloperDir() (string, error) {
	out, err := exec.Command("xcode-select", "-p").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

var (
	ErrNotDevice   = errkind.New(errkind.Invalid, "this Mac can't be booted or shut down from here")
	ErrUnknown     = errkind.New(errkind.NotFound, "no simulator with that id on this machine")
	ErrNotRunning  = errkind.New(errkind.Unavailable, "the simulator isn't running")
	ErrBootTimeout = errkind.New(errkind.Unavailable, "the emulator didn't finish booting in time")
)

// bootWait bounds an Android cold boot; most finish in under a minute.
const bootWait = 3 * time.Minute

// Boot starts a device and returns once it is ready to use. An Android
// emulator runs without a window, in its own process group, so it outlives
// this call and a host restart.
func Boot(ctx context.Context, id string) error {
	if id == DisplayID {
		return ErrNotDevice
	}
	d, err := find(ctx, id)
	if err != nil || d.Booted {
		return err
	}
	if d.Platform == "ios" {
		// bootstatus -b boots and waits until the simulator has finished starting.
		_, err := runFor(ctx, bootWait, "xcrun", "simctl", "bootstatus", d.ID, "-b")
		return err
	}
	cmd := exec.Command(androidTool("emulator", "emulator"), "-avd", d.ID, "-no-window", "-no-audio", "-no-boot-anim")
	proctree.Own(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return waitForAndroid(ctx, d.ID)
}

// Shutdown stops a running device; one already stopped is left alone.
func Shutdown(ctx context.Context, id string) error {
	if id == DisplayID {
		return ErrNotDevice
	}
	d, err := find(ctx, id)
	if err != nil || !d.Booted {
		return err
	}
	if d.Platform == "ios" {
		_, err := run(ctx, "xcrun", "simctl", "shutdown", d.ID)
		return err
	}
	_, err = run(ctx, androidTool("platform-tools", "adb"), "-s", d.Serial, "emu", "kill")
	return err
}

const timeout = 10 * time.Second

// run returns stdout only, so a tool's warnings on stderr can't break the JSON.
func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return runFor(ctx, timeout, name, args...)
}

func runFor(ctx context.Context, limit time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, errors.New(filepath.Base(name) + ": " + strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

// runInput runs name with stdin as its input.
func runInput(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return nil, errors.New(filepath.Base(name) + ": " + strings.TrimSpace(string(exit.Stderr)))
		}
		return nil, err
	}
	return out, nil
}

// readFile reads dir/name as text.
func readFile(dir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	return string(b), err
}
