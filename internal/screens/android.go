package screens

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/repogoapp/host/internal/screens/capture"
	"github.com/repogoapp/host/internal/screens/emulator"
)

// ListAndroid reads the AVDs from `emulator -list-avds` and marks the running
// ones from `adb devices`, asking each emulator which AVD it is.
func ListAndroid(ctx context.Context) ([]Screen, error) {
	emulator := androidTool("emulator", "emulator")
	if emulator == "" {
		return nil, nil
	}
	out, err := run(ctx, emulator, "-list-avds")
	if err != nil {
		return nil, err
	}
	var screens []Screen
	for _, avd := range strings.Fields(string(out)) {
		screens = append(screens, Screen{
			Kind:     KindEmulator,
			Platform: "android",
			ID:       avd,
			Name:     strings.ReplaceAll(avd, "_", " "),
			OS:       avdAPILevel(avd),
		})
	}
	running := runningAVDs(ctx)
	for i := range screens {
		if serial, ok := running[screens[i].ID]; ok {
			screens[i].Booted, screens[i].Serial = true, serial
		}
	}
	sortDevices(screens)
	return screens, nil
}

// runningAVDs maps AVD name to adb serial for each running emulator.
func runningAVDs(ctx context.Context) map[string]string {
	adb := androidTool("platform-tools", "adb")
	if adb == "" {
		return nil
	}
	out, err := run(ctx, adb, "devices")
	if err != nil {
		return nil
	}
	running := map[string]string{}
	for _, serial := range emulatorSerials(string(out)) {
		name, err := run(ctx, adb, "-s", serial, "emu", "avd", "name")
		if err != nil {
			continue
		}
		// The console answers the name, then "OK".
		if avd, _, _ := strings.Cut(strings.TrimSpace(string(name)), "\n"); avd != "" {
			running[strings.TrimSpace(avd)] = serial
		}
	}
	return running
}

// emulatorSerials picks the online emulators out of `adb devices`.
func emulatorSerials(out string) []string {
	var serials []string
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.HasPrefix(fields[0], "emulator-") && fields[1] == "device" {
			serials = append(serials, fields[0])
		}
	}
	return serials
}

// avdAPILevel reads the AVD's target from its .ini ("android-37.0" → "API 37.0").
func avdAPILevel(avd string) string {
	home := avdHome()
	if home == "" {
		return ""
	}
	ini, err := os.ReadFile(filepath.Join(home, avd+".ini"))
	if err != nil {
		return ""
	}
	if level, ok := strings.CutPrefix(iniValue(string(ini), "target"), "android-"); ok {
		return "API " + level
	}
	return ""
}

func avdHome() string {
	if dir := os.Getenv("ANDROID_AVD_HOME"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".android", "avd")
}

func androidSDKs() []string {
	sdks := []string{os.Getenv("ANDROID_HOME"), os.Getenv("ANDROID_SDK_ROOT")}
	if home, err := os.UserHomeDir(); err == nil {
		switch runtime.GOOS {
		case "darwin":
			sdks = append(sdks, filepath.Join(home, "Library", "Android", "sdk"))
		case "windows":
			sdks = append(sdks, filepath.Join(os.Getenv("LOCALAPPDATA"), "Android", "Sdk"))
		default:
			sdks = append(sdks, filepath.Join(home, "Android", "Sdk"))
		}
	}
	var found []string
	for _, sdk := range sdks {
		if sdk != "" {
			found = append(found, sdk)
		}
	}
	return found
}

// androidTool finds an SDK tool on PATH or in the SDK's sub folder.
func androidTool(sub, name string) string {
	if path, err := exec.LookPath(name); err == nil {
		return path
	}
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	for _, sdk := range androidSDKs() {
		path := filepath.Join(sdk, sub, name)
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return ""
}

func iniValue(ini, key string) string {
	for _, line := range strings.Split(ini, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// waitForAndroid polls until the AVD's emulator reports boot completed.
func waitForAndroid(ctx context.Context, avd string) error {
	ctx, cancel := context.WithTimeout(ctx, bootWait)
	defer cancel()
	adb := androidTool("platform-tools", "adb")
	for {
		if serial, ok := runningAVDs(ctx)[avd]; ok {
			out, err := run(ctx, adb, "-s", serial, "shell", "getprop", "sys.boot_completed")
			if err == nil && strings.TrimSpace(string(out)) == "1" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ErrBootTimeout
		case <-time.After(2 * time.Second):
		}
	}
}

// streamAndroid streams an emulator as a simulator streams on a Mac: `repogo
// androidcap` reads its display from its gRPC control port and encodes it
// here, and plays input through the same port. Elsewhere, or for an emulator
// without that port, it falls back to screenrecord over adb.
func streamAndroid(ctx context.Context, c *controller, send func([]byte) error, input <-chan capture.Input) error {
	if port, ok := consolePort(c.d.Serial); ok && runtime.GOOS == "darwin" {
		if _, err := emulator.Find(port); err == nil {
			return streamChild(ctx, c, send, input, []string{"androidcap", strconv.Itoa(port)}, nil)
		}
	}
	return recordAndroid(ctx, c, send, input)
}

// consolePort is an emulator's console port, from its serial (emulator-5554).
func consolePort(serial string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(serial, "emulator-"))
	return n, err == nil && strings.HasPrefix(serial, "emulator-")
}

// recordAndroid runs screenrecord, again when it stops on its own every few
// minutes or the screen changes size (a rotation, a fold).
func recordAndroid(ctx context.Context, c *controller, send func([]byte) error, input <-chan capture.Input) error {
	touches := make(chan capture.Input)
	go playAndroid(ctx, c, touches)
	go func() {
		defer close(touches)
		c.serve(ctx, input, func(in capture.Input) error {
			select {
			case touches <- in:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	for {
		started := time.Now()
		run, cancel := context.WithCancel(ctx)
		var restarted atomic.Bool
		go func() {
			select {
			case <-c.restart:
				restarted.Store(true)
				cancel()
			case <-run.Done():
			}
		}()
		err := pump(run, exec.CommandContext(run, c.adb, "-s", c.d.Serial, "exec-out", "screenrecord",
			"--output-format=h264", "--bit-rate", streamBitRate, "--size", streamSize(c.screen(ctx)), "-"), send)
		cancel()
		switch {
		case ctx.Err() != nil:
			return nil
		case restarted.Load():
			continue
		case err != nil:
			return err
		case time.Since(started) < minRecording:
			return errors.New("screenrecord stopped as soon as it started")
		}
	}
}

const (
	// streamBitRate is what screenrecord encodes at: a few Mbps, which a
	// tailnet or LAN carries easily.
	streamBitRate = "4000000"
	// maxShortSide caps the video's short side, as simcap does: past 720 px,
	// encoding and decoding cost latency for little gain in legibility.
	maxShortSide = 720
	// idleFlush is how long screenrecord may go quiet before the bytes held
	// are taken as a whole frame. A frame's bytes arrive together, so a short
	// wait ends it; otherwise it would wait for the next frame, which may not
	// come until the screen changes again.
	idleFlush = 5 * time.Millisecond
	// minRecording is the shortest run that counts as working: screenrecord
	// stops itself every few minutes and is restarted, but one that dies at
	// once is failing, not finished.
	minRecording = 5 * time.Second
)

// streamSize is screenrecord's --size: the screen scaled so its short side is
// at most maxShortSide, in even pixels; the native size when unknown.
func streamSize(w, h int) string {
	if w <= 0 || h <= 0 {
		return "0x0"
	}
	scale := math.Min(1, float64(maxShortSide)/float64(min(w, h)))
	even := func(n int) int { return int(float64(n)*scale) &^ 1 }
	return fmt.Sprintf("%dx%d", even(w), even(h))
}

// screenSize reads the emulator's screen in pixels from `wm size`, preferring
// an override; zero when it can't.
func screenSize(ctx context.Context, adb, serial string) (w, h int) {
	out, err := run(ctx, adb, "-s", serial, "shell", "wm", "size")
	if err != nil {
		return 0, 0
	}
	return parseWMSize(string(out))
}

// parseWMSize reads "Physical size: 1080x2400" and an "Override size" after it.
func parseWMSize(out string) (w, h int) {
	for _, line := range strings.Split(out, "\n") {
		_, size, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		ws, hs, ok := strings.Cut(strings.TrimSpace(size), "x")
		if !ok {
			continue
		}
		if a, err := strconv.Atoi(ws); err == nil {
			if b, err := strconv.Atoi(hs); err == nil {
				w, h = a, b
			}
		}
	}
	return w, h
}

// playAndroid plays touches and buttons on the emulator through one adb
// shell kept open, as they happen: a press, its drag and its lift as motion
// events, so a scroll follows the finger. Each `input` command takes tens of
// milliseconds, so moves go at most every moveGap, the latest one winning.
func playAndroid(ctx context.Context, c *controller, input <-chan capture.Input) {
	w, h := c.screen(ctx)
	if w == 0 {
		return
	}
	cmd := exec.CommandContext(ctx, c.adb, "-s", c.d.Serial, "shell")
	shell, err := cmd.StdinPipe()
	if err != nil || cmd.Start() != nil {
		return
	}
	defer cmd.Wait()
	defer shell.Close()
	write := func(line string) bool {
		_, err := io.WriteString(shell, line+"\n")
		return err == nil
	}
	var pending *capture.Input
	// Typed text not yet sent: each `input` call starts a process on the
	// device, so a burst of keys goes as one, before anything else.
	var typed strings.Builder
	flush := func() bool {
		if typed.Len() == 0 {
			return true
		}
		lines := androidText(typed.String())
		typed.Reset()
		for _, line := range lines {
			if !write(line) {
				return false
			}
		}
		return true
	}
	burst := time.NewTimer(time.Hour)
	defer burst.Stop()
	// The finger's last point while it is down: a viewer that closes
	// mid-touch never lifts it, so the next press, or the end, lifts it first.
	var held *capture.Input
	defer func() {
		if held != nil {
			held.Touch = "up"
			write(androidCommand(*held, w, h))
		}
	}()
	gap := time.NewTimer(time.Hour)
	defer gap.Stop()
	ready := true
	for {
		select {
		case in, ok := <-input:
			if !ok {
				flush()
				return
			}
			if in.Action == capture.ActionText {
				typed.WriteString(in.Value)
				burst.Reset(typingBurst)
				continue
			}
			if in.Key != nil && !in.Key.Down {
				continue
			}
			if !flush() {
				return
			}
			if in.Key != nil {
				if line := androidKey(in.Key.Keysym); line != "" && !write(line) {
					return
				}
				continue
			}
			switch in.Touch {
			case "move":
				if !ready {
					pending, held = &in, &in
					continue
				}
				ready = false
				gap.Reset(moveGap)
			case "down":
				if held != nil {
					lift := *held
					lift.Touch = "up"
					write(androidCommand(lift, w, h))
				}
				// A rotation or fold since the last press changed the screen.
				if c.sizeChanged.Swap(false) {
					w, h = c.screen(ctx)
				}
			case "up", "cancel":
				// A lift carries the finger's last position; a held move is stale.
				pending = nil
			}
			switch in.Touch {
			case "down", "move":
				held = &in
			case "up", "cancel":
				held = nil
			}
			if line := androidCommand(in, w, h); line != "" && !write(line) {
				return
			}
		case <-burst.C:
			if !flush() {
				return
			}
		case <-gap.C:
			ready = true
			if pending != nil {
				in := *pending
				pending = nil
				ready = false
				gap.Reset(moveGap)
				if !write(androidCommand(in, w, h)) {
					return
				}
			}
		}
	}
}

// moveGap is how often a drag's position goes to the emulator.
const moveGap = 40 * time.Millisecond

// typingBurst is how long typed text waits for more before it goes.
const typingBurst = 40 * time.Millisecond

// androidCommand is the `input` command for one touch or button; empty for
// anything the emulator has no command for.
func androidCommand(in capture.Input, w, h int) string {
	px := func(f float64, size int) int { return int(math.Round(f * float64(size-1))) }
	switch in.Touch {
	case "down", "move", "up", "cancel":
		return fmt.Sprintf("input motionevent %s %d %d", strings.ToUpper(in.Touch), px(in.X, w), px(in.Y, h))
	case "":
		if key, ok := androidKeys[in.Button]; ok {
			return "input keyevent " + key
		}
	}
	return ""
}

// androidText is the shell lines that type text: spaces as %s, which
// `input text` turns back into spaces. It has no escape for a literal "%s",
// so text is split between the two characters.
func androidText(text string) []string {
	var lines []string
	parts := strings.Split(text, "%s")
	for i, part := range parts {
		if i > 0 {
			part = "s" + part
		}
		if i < len(parts)-1 {
			part += "%"
		}
		if part != "" {
			lines = append(lines, "input text "+shellQuote(strings.ReplaceAll(part, " ", "%s")))
		}
	}
	return lines
}

// androidKey is the shell line that presses a keysym's key: the control
// keys and arrows; a character is typed; modifiers have no key of their own.
func androidKey(keysym uint32) string {
	if code, ok := androidKeycodes[keysym]; ok {
		return "input keyevent " + code
	}
	if keysym >= 0x20 && keysym <= 0x7e {
		return androidText(string(rune(keysym)))[0]
	}
	return ""
}

var androidKeycodes = map[uint32]string{
	0xff08: "KEYCODE_DEL", 0xffff: "KEYCODE_FORWARD_DEL", 0xff09: "KEYCODE_TAB", 0xff0d: "KEYCODE_ENTER",
	0xff1b: "KEYCODE_ESCAPE", 0xff51: "KEYCODE_DPAD_LEFT", 0xff52: "KEYCODE_DPAD_UP",
	0xff53: "KEYCODE_DPAD_RIGHT", 0xff54: "KEYCODE_DPAD_DOWN",
}

var androidKeys = map[string]string{
	"home": "KEYCODE_HOME", "back": "KEYCODE_BACK", "recents": "KEYCODE_APP_SWITCH", "lock": "KEYCODE_POWER",
}

func (c *controller) readAndroid(ctx context.Context) {
	if out, err := c.shell(ctx, "cmd uimode night"); err == nil {
		c.state.Appearance = map[bool]string{true: "dark", false: "light"}[strings.Contains(string(out), "yes")]
	}
	if out, err := c.shell(ctx, "settings get system font_scale"); err == nil {
		scale, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
		c.state.ContentSize = nearestSize(scale)
	}
	if out, err := c.shell(ctx, "settings get secure high_text_contrast_enabled"); err == nil {
		c.state.Contrast = strings.TrimSpace(string(out)) == "1"
	}
	if c.hasHinge() {
		if out, err := c.shell(ctx, "cmd device_state state"); err == nil {
			c.state.Folded = strings.Contains(string(out), "CLOSED")
		}
	}
}

// hasHinge reads the AVD's config: a hinge sensor makes it a foldable.
func (c *controller) hasHinge() bool {
	home := avdHome()
	ini, err := readFile(home, c.d.ID+".ini")
	if err != nil {
		return false
	}
	config, err := readFile(iniValue(ini, "path"), "config.ini")
	return err == nil && iniValue(config, "hw.sensor.hinge") == "yes"
}

func (c *controller) shell(ctx context.Context, command string) ([]byte, error) {
	return run(ctx, c.adb, "-s", c.d.Serial, "shell", command)
}

// screen is the emulator's screen as touches address it now: its physical
// size, swapped while it lies on its side.
func (c *controller) screen(ctx context.Context) (w, h int) {
	w, h = screenSize(ctx, c.adb, c.d.Serial)
	c.mu.Lock()
	sideways := c.state.Orientation == 3 || c.state.Orientation == 4
	c.mu.Unlock()
	if sideways {
		return h, w
	}
	return w, h
}

func (c *controller) restartRecorder() {
	c.sizeChanged.Store(true)
	select {
	case c.restart <- struct{}{}:
	default:
	}
}

func (c *controller) actAndroid(ctx context.Context, in capture.Input) error {
	var err error
	switch in.Action {
	case "appearance":
		mode := map[bool]string{true: "yes", false: "no"}[lightOrDark(in.Value) == "dark"]
		if _, err = c.shell(ctx, "cmd uimode night "+mode); err == nil {
			c.set(func(s *State) { s.Appearance = lightOrDark(in.Value) })
		}
	case "content_size":
		size, ok := contentSize(in.Value)
		if !ok {
			return fmt.Errorf("unknown text size %q", in.Value)
		}
		if _, err = c.shell(ctx, fmt.Sprintf("settings put system font_scale %g", scaleOf(size))); err == nil {
			c.set(func(s *State) { s.ContentSize = size })
		}
	case "contrast":
		on := in.Value == "on"
		if _, err = c.shell(ctx, "settings put secure high_text_contrast_enabled "+map[bool]string{true: "1", false: "0"}[on]); err == nil {
			c.set(func(s *State) { s.Contrast = on })
		}
	case "rotate":
		// The emulator turns clockwise a quarter at a time.
		turns := map[string]int{"right": 1, "left": 3}[in.Value]
		for range turns {
			if _, err = run(ctx, c.adb, "-s", c.d.Serial, "emu", "rotate"); err != nil {
				return err
			}
		}
		c.set(func(s *State) { s.Orientation = turn(s.Orientation, in.Value) })
		c.restartRecorder()
	case "fold":
		folded := in.Value == "closed"
		if _, err = run(ctx, c.adb, "-s", c.d.Serial, "emu", map[bool]string{true: "fold", false: "unfold"}[folded]); err == nil {
			c.set(func(s *State) { s.Folded = folded })
			c.restartRecorder()
		}
	case "url":
		_, err = c.shell(ctx, "am start -a android.intent.action.VIEW -d "+shellQuote(in.Value))
	case "location":
		if in.Value != "clear" {
			_, err = run(ctx, c.adb, "-s", c.d.Serial, "emu", "geo", "fix", fmt.Sprint(in.Lon), fmt.Sprint(in.Lat))
		}
	case "status_bar":
		on := in.Value == "on"
		demo := "am broadcast -a com.android.systemui.demo -e command "
		script := demo + "exit"
		if on {
			script = strings.Join([]string{"settings put global sysui_demo_allowed 1", demo + "enter",
				demo + "clock -e hhmm 0941", demo + "battery -e level 100 -e plugged false",
				demo + "network -e wifi show -e level 4 -e mobile show -e datatype none -e level 4",
				demo + "notifications -e visible false"}, "; ")
		}
		if _, err = c.shell(ctx, script); err == nil {
			c.set(func(s *State) { s.StatusBar = on })
		}
	case "slow_animations":
		on := in.Value == "on"
		scale := map[bool]string{true: "10", false: "1"}[on]
		if _, err = c.shell(ctx, "settings put global animator_duration_scale "+scale+
			"; settings put global transition_animation_scale "+scale+
			"; settings put global window_animation_scale "+scale); err == nil {
			c.set(func(s *State) { s.SlowAnimations = on })
		}
	case "screenshot":
		var shot []byte
		if shot, err = run(ctx, c.adb, "-s", c.d.Serial, "exec-out", "screencap", "-p"); err == nil {
			c.reply(Message{Screenshot: base64.StdEncoding.EncodeToString(shot)})
		}
	default:
		return fmt.Errorf("%s isn't available on this emulator", in.Action)
	}
	return err
}

// nearestSize is the content size category whose Android scale is closest.
func nearestSize(scale float64) string {
	best := contentSizes[0]
	for _, s := range contentSizes[1:] {
		if math.Abs(s.scale-scale) < math.Abs(best.scale-scale) {
			best = s
		}
	}
	return best.name
}

func scaleOf(name string) float64 {
	for _, s := range contentSizes {
		if s.name == name {
			return s.scale
		}
	}
	return 1
}
