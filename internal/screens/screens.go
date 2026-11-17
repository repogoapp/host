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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/proctree"
)

// Screen is one live screen: a simulator, an emulator or this machine's display.
type Screen struct {
	Kind     Kind        `json:"kind"`
	Platform Platform    `json:"platform"`
	ID       string      `json:"id"` // simulator UDID, AVD name, or "display"
	Name     string      `json:"name"`
	OS       OS          `json:"os"`
	State    ScreenState `json:"state"`
	Form     Form        `json:"form"`
	// User is the display's account the host signs in as, for the phone to name.
	User string `json:"user,omitempty"`

	// serial is the adb serial (emulator-5554) while an emulator runs.
	serial string
	// data is a simulator's data folder.
	data string
	// run names the run a controller was made for; see runOf.
	run string
}

// Booted reports whether the screen is running.
func (s Screen) Booted() bool { return s.State == StateBooted }

// List returns every available device, iOS first, each platform sorted by
// name then newest OS, then this Mac while Screen Sharing is on. An error
// from one platform is returned alongside the other platforms' screens.
func List(ctx context.Context) ([]Screen, error) {
	ios, iosErr := ListIOS(ctx)
	android, androidErr := ListAndroid(ctx)
	screens := append(ios, android...)
	if mac, ok := ThisDisplay(ctx); ok {
		screens = append(screens, mac)
	}
	transitions.overlay(screens)
	return screens, errors.Join(iosErr, androidErr)
}

func sortDevices(screens []Screen) {
	sort.SliceStable(screens, func(i, j int) bool {
		if screens[i].Name != screens[j].Name {
			return screens[i].Name < screens[j].Name
		}
		return newerVersion(screens[i].OS.Version, screens[j].OS.Version)
	})
}

// newerVersion compares dotted versions by number: 27.1 is newer than 9.3.
func newerVersion(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(as), len(bs)); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
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

// transitions are the boots and shutdowns this host started and hasn't
// seen finish, which List reports in place of the tools' state.
var transitions = transitionSet{m: map[string]ScreenState{}, turns: map[string]chan struct{}{}}

type transitionSet struct {
	mu sync.Mutex
	m  map[string]ScreenState
	// turns holds a screen's boots and shutdowns to one at a time.
	turns map[string]chan struct{}
}

// begin waits for the screen's boot or shutdown under way, if any, then
// takes its turn until end; an error when ctx ends first.
func (t *transitionSet) begin(ctx context.Context, id string) (end func(), err error) {
	t.mu.Lock()
	turn := t.turns[id]
	if turn == nil {
		turn = make(chan struct{}, 1)
		t.turns[id] = turn
	}
	t.mu.Unlock()
	select {
	case turn <- struct{}{}:
		return func() { <-turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (t *transitionSet) set(id string, s ScreenState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if s == "" {
		delete(t.m, id)
	} else {
		t.m[id] = s
	}
}

func (t *transitionSet) overlay(screens []Screen) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range screens {
		if s, ok := t.m[screens[i].ID]; ok {
			screens[i].State = s
		}
	}
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
	ErrUnknown     = errkind.New(errkind.NotFound, "no screen with that id on this machine")
	ErrBootTimeout = errkind.New(errkind.Unavailable, "the emulator didn't finish booting in time")
)

// bootWait bounds an Android cold boot; most finish in under a minute.
const bootWait = 3 * time.Minute

// Boot starts a device and returns once it is ready to use: after a boot or
// shutdown of it already under way here, which it waits for. An Android
// emulator runs without a window, in its own process group, so it outlives
// this call and a host restart.
func Boot(ctx context.Context, id string) error {
	if id == DisplayID {
		return ErrNotDevice
	}
	end, err := transitions.begin(ctx, id)
	if err != nil {
		return err
	}
	defer end()
	d, err := find(ctx, id)
	// A simulator someone else is booting is waited for too.
	booting := d.State == StateBooting && d.Platform == PlatformIOS
	if err != nil || (d.State != StateShutdown && !booting) {
		return err
	}
	transitions.set(id, StateBooting)
	defer transitions.set(id, "")
	if d.Platform == PlatformIOS {
		// bootstatus -b boots and waits until the simulator has finished starting.
		_, err := runFor(ctx, bootWait, "", "xcrun", "simctl", "bootstatus", d.ID, "-b")
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

// Shutdown stops a running device, after a boot of it under way here; one
// already stopped is left alone.
func Shutdown(ctx context.Context, id string) error {
	if id == DisplayID {
		return ErrNotDevice
	}
	end, err := transitions.begin(ctx, id)
	if err != nil {
		return err
	}
	defer end()
	d, err := find(ctx, id)
	if err != nil || d.State != StateBooted {
		return err
	}
	transitions.set(id, StateShuttingDown)
	defer transitions.set(id, "")
	if d.Platform == PlatformIOS {
		_, err := run(ctx, "xcrun", "simctl", "shutdown", d.ID)
		return err
	}
	_, err = run(ctx, androidTool("platform-tools", "adb"), "-s", d.serial, "emu", "kill")
	return err
}

const timeout = 10 * time.Second

// run returns stdout only, so a tool's warnings on stderr can't break the JSON.
func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return runFor(ctx, timeout, "", name, args...)
}

// runInput runs name with stdin as its input.
func runInput(ctx context.Context, stdin, name string, args ...string) ([]byte, error) {
	return runFor(ctx, timeout, stdin, name, args...)
}

func runFor(ctx context.Context, limit time.Duration, stdin, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
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
