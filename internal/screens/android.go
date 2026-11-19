package screens

import (
	"context"
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
		name := strings.ReplaceAll(avd, "_", " ")
		form := FormPhone
		switch {
		case hasHinge(avd):
			form = FormFoldable
		case strings.Contains(strings.ToLower(name), "tablet"):
			form = FormTablet
		}
		screens = append(screens, Screen{
			Kind:     KindEmulator,
			Platform: PlatformAndroid,
			ID:       avd,
			Name:     name,
			OS:       avdOS(avd),
			State:    StateShutdown,
			Form:     form,
		})
	}
	running := runningAVDs(ctx)
	for i := range screens {
		if serial, ok := running[screens[i].ID]; ok {
			screens[i].State, screens[i].serial = StateBooted, serial
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

// avdOS reads the AVD's target from its .ini ("android-36" is Android 16, API 36).
func avdOS(avd string) OS {
	os := OS{Name: "Android"}
	ini, err := readFile(avdHome(), avd+".ini")
	if err != nil {
		return os
	}
	level, ok := strings.CutPrefix(iniValue(ini, "target"), "android-")
	major, _, _ := strings.Cut(level, ".")
	n, err := strconv.Atoi(major)
	if !ok || err != nil {
		return os
	}
	os.APILevel = &n
	os.Version = androidReleases[n]
	return os
}

// androidReleases are Android's versions by API level.
var androidReleases = map[int]string{
	26: "8.0", 27: "8.1", 28: "9", 29: "10", 30: "11", 31: "12", 32: "12L", 33: "13", 34: "14",
	35: "15", 36: "16", 37: "17",
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
func streamAndroid(ctx context.Context, c *controller, framed *framer, input *inputQueue) error {
	if port, ok := consolePort(c.d.serial); ok && runtime.GOOS == "darwin" {
		if _, err := emulator.Find(port); err == nil {
			return streamChild(ctx, c, framed.child, framed.pace, input, []string{"androidcap", strconv.Itoa(port)}, nil)
		}
	}
	return recordAndroid(ctx, c, framed, input)
}

// consolePort is an emulator's console port, from its serial (emulator-5554).
func consolePort(serial string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimPrefix(serial, "emulator-"))
	return n, err == nil && strings.HasPrefix(serial, "emulator-")
}

// recordAndroid runs screenrecord, again when it stops on its own every few
// minutes or the screen changes size (a rotation, a fold). Each run's frames
// go through framed, so their sequence keeps counting.
func recordAndroid(ctx context.Context, c *controller, framed *framer, input *inputQueue) error {
	touches := make(chan capture.Input)
	go playAndroid(ctx, c, touches)
	toDevice := func(in capture.Input) error {
		if in.Action == capture.ActionKeyframe {
			// screenrecord has no way to ask; started again, it opens with one.
			c.restartRecorder()
			return nil
		}
		select {
		case touches <- in:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	defer c.open(toDevice)()
	go func() {
		defer close(touches)
		c.serve(ctx, input, toDevice)
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
		err := pump(run, exec.CommandContext(run, c.adb, "-s", c.d.serial, "exec-out", "screenrecord",
			"--output-format=h264", "--bit-rate", streamBitRate, "--size", streamSize(c.screen(ctx)), "-"), framed)
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
	cmd := exec.CommandContext(ctx, c.adb, "-s", c.d.serial, "shell")
	// The lift below has to reach the shell after ctx ends.
	endsWithInput(cmd)
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

func (c *controller) readAndroid(ctx context.Context) State {
	var s State
	if out, err := c.shell(ctx, "cmd uimode night"); err == nil {
		v := AppearanceLight
		if strings.Contains(string(out), "yes") {
			v = AppearanceDark
		}
		s.Appearance = &v
	}
	if out, err := c.shell(ctx, "settings get system font_scale"); err == nil {
		if scale, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64); err == nil {
			s.ContentSize = ptr(nearestSize(scale))
		}
	}
	if out, err := c.shell(ctx, "settings get secure high_text_contrast_enabled"); err == nil {
		s.Contrast = ptr(strings.TrimSpace(string(out)) == "1")
	}
	if c.foldable {
		if out, err := c.shell(ctx, "cmd device_state state"); err == nil {
			pose := PoseOpen
			if strings.Contains(string(out), "CLOSED") {
				pose = PoseClosed
			}
			s.Pose = &pose
		}
	}
	return s
}

// hasHinge reads the AVD's config: a hinge sensor makes it a foldable.
func hasHinge(avd string) bool {
	ini, err := readFile(avdHome(), avd+".ini")
	if err != nil {
		return false
	}
	config, err := readFile(iniValue(ini, "path"), "config.ini")
	return err == nil && iniValue(config, "hw.sensor.hinge") == "yes"
}

func (c *controller) shell(ctx context.Context, command string) ([]byte, error) {
	return run(ctx, c.adb, "-s", c.d.serial, "shell", command)
}

// screen is the emulator's screen as touches address it now: its physical
// size, swapped while it lies on its side.
func (c *controller) screen(ctx context.Context) (w, h int) {
	w, h = screenSize(ctx, c.adb, c.d.serial)
	c.mu.Lock()
	sideways := c.orientation == 3 || c.orientation == 4
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

func (c *controller) setAndroid(ctx context.Context, s Settings) error {
	if v := s.Appearance; v != nil {
		if _, err := c.shell(ctx, "cmd uimode night "+map[bool]string{true: "yes", false: "no"}[*v == AppearanceDark]); err != nil {
			return err
		}
		c.update(func(st *State) { st.Appearance = ptr(*v) })
	}
	if v := s.ContentSize; v != nil {
		if _, err := c.shell(ctx, fmt.Sprintf("settings put system font_scale %g", scaleOf(*v))); err != nil {
			return err
		}
		c.update(func(st *State) { st.ContentSize = ptr(*v) })
	}
	if v := s.Contrast; v != nil {
		if _, err := c.shell(ctx, "settings put secure high_text_contrast_enabled "+map[bool]string{true: "1", false: "0"}[*v]); err != nil {
			return err
		}
		c.update(func(st *State) { st.Contrast = ptr(*v) })
	}
	if v := s.StatusBar; v != nil {
		demo := "am broadcast -a com.android.systemui.demo -e command "
		script := demo + "exit"
		if *v {
			script = strings.Join([]string{"settings put global sysui_demo_allowed 1", demo + "enter",
				demo + "clock -e hhmm 0941", demo + "battery -e level 100 -e plugged false",
				demo + "network -e wifi show -e level 4 -e mobile show -e datatype none -e level 4",
				demo + "notifications -e visible false"}, "; ")
		}
		if _, err := c.shell(ctx, script); err != nil {
			return err
		}
		c.update(func(st *State) { st.StatusBar = ptr(*v) })
	}
	if v := s.SlowAnimations; v != nil {
		scale := map[bool]string{true: "10", false: "1"}[*v]
		if _, err := c.shell(ctx, "settings put global animator_duration_scale "+scale+
			"; settings put global transition_animation_scale "+scale+
			"; settings put global window_animation_scale "+scale); err != nil {
			return err
		}
		c.update(func(st *State) { st.SlowAnimations = ptr(*v) })
	}
	if v := s.Pose; v != nil {
		// An emulator folds closed or opens; book and laptop are open.
		folded := *v == PoseClosed
		if _, err := run(ctx, c.adb, "-s", c.d.serial, "emu", map[bool]string{true: "fold", false: "unfold"}[folded]); err != nil {
			return err
		}
		pose := PoseOpen
		if folded {
			pose = PoseClosed
		}
		c.update(func(st *State) { st.Pose = &pose })
		c.restartRecorder()
	}
	return nil
}

func (c *controller) rotateAndroid(ctx context.Context, t Turn) error {
	// The emulator turns clockwise a quarter at a time.
	turns := map[Turn]int{TurnRight: 1, TurnLeft: 3}[t]
	for range turns {
		if _, err := run(ctx, c.adb, "-s", c.d.serial, "emu", "rotate"); err != nil {
			return err
		}
	}
	c.mu.Lock()
	c.orientation = turn(c.orientation, t)
	c.mu.Unlock()
	c.restartRecorder()
	return nil
}

func (c *controller) locationAndroid(ctx context.Context, at *Coordinate) error {
	// The emulator has no way to clear a fix; the last one stays.
	if at == nil {
		return nil
	}
	_, err := run(ctx, c.adb, "-s", c.d.serial, "emu", "geo", "fix", fmt.Sprint(at.Lon), fmt.Sprint(at.Lat))
	return err
}

// nearestSize is the content size category whose Android scale is closest.
func nearestSize(scale float64) ContentSize {
	best := contentSizes[0]
	for _, s := range contentSizes[1:] {
		if math.Abs(s.scale-scale) < math.Abs(best.scale-scale) {
			best = s
		}
	}
	return ContentSize(best.name)
}

func scaleOf(size ContentSize) float64 {
	for _, s := range contentSizes {
		if s.name == string(size) {
			return s.scale
		}
	}
	return 1
}
