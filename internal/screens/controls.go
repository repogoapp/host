package screens

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/screens/capture"
	"github.com/repogoapp/host/internal/screens/emulator"
)

// Events is the part of the host's emitter the screens use: each screen's
// live view is a room, screen:<id>, holding its latest view and desktop.
type Events interface {
	Publish(r emit.Room, ev emit.Stateful, judge func(cur emit.Event, ok bool) emit.Verdict) bool
	Join(d device.ID, r emit.Room) emit.Snapshot
	Leave(d device.ID, r emit.Room)
}

// Manager keeps one controller per screen a phone looks at or acts on, so
// every phone watching a screen sees the same settings.
type Manager struct {
	events Events

	mu          sync.Mutex
	controllers map[string]*controller
}

func NewManager(events Events) *Manager {
	return &Manager{events: events, controllers: map[string]*controller{}}
}

func room(id string) emit.Room { return emit.Room("screen:" + id) }

// controller is the running screen's controller, made and read on first use.
func (m *Manager) controller(ctx context.Context, id string) (*controller, error) {
	c, _, err := m.lookup(ctx, id)
	return c, err
}

// current is the screen's controller with its settings just read, so a
// change made outside the app shows.
func (m *Manager) current(ctx context.Context, id string) (*controller, error) {
	c, made, err := m.lookup(ctx, id)
	if err == nil && !made {
		c.read(ctx)
	}
	return c, err
}

// lookup finds the screen's controller, or makes and reads it: made says
// which. One with a live view open is running, so it skips listing the
// devices. A screen seen shut down, or started again since, loses its
// controller: what the host set on it went with that run.
func (m *Manager) lookup(ctx context.Context, id string) (c *controller, made bool, err error) {
	m.mu.Lock()
	if c := m.controllers[id]; c != nil && c.streaming() {
		m.mu.Unlock()
		return c, false, nil
	}
	m.mu.Unlock()
	d, err := find(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if !d.Booted() {
		m.forget(id)
		return nil, false, ErrNotRunning
	}
	d.run = runOf(d)
	m.mu.Lock()
	c = m.controllers[id]
	if c != nil && c.d.serial == d.serial && c.d.run == d.run {
		m.mu.Unlock()
		return c, false, nil
	}
	c = newController(d, m)
	m.controllers[id] = c
	m.mu.Unlock()
	c.read(ctx)
	return c, true, nil
}

// runOf names this run of a booted screen, so one started again outside the
// host is told from the last: the time a simulator's launchd started, or
// the emulator's process. Empty when neither can be read.
func runOf(d Screen) string {
	switch d.Kind {
	case KindSimulator:
		if d.data == "" {
			return ""
		}
		if info, err := os.Stat(filepath.Join(d.data, "var", "run", "launchd_bootstrap.plist")); err == nil {
			return info.ModTime().String()
		}
	case KindEmulator:
		if port, ok := consolePort(d.serial); ok {
			return emulator.Run(port)
		}
	}
	return ""
}

// forget drops a screen's controller: it shut down.
func (m *Manager) forget(id string) {
	m.mu.Lock()
	c := m.controllers[id]
	delete(m.controllers, id)
	m.mu.Unlock()
	if c != nil {
		c.closeAgent()
	}
}

// ViewResult is screens.view's reply: the room's view and desktop as the
// device joined it. Later pushes carry newer revisions.
type ViewResult struct {
	View    *ViewChanged    `json:"view,omitempty"`
	Desktop *DesktopChanged `json:"desktop,omitempty"`
}

// View joins the device to the screen's room and returns its state, read
// again so a change made outside the app shows.
func (m *Manager) View(ctx context.Context, d device.ID, id string) (ViewResult, error) {
	if _, err := m.current(ctx, id); err != nil {
		return ViewResult{}, err
	}
	snap := m.events.Join(d, room(id))
	var out ViewResult
	if v, ok := snap.State["view"].(ViewChanged); ok {
		out.View = &v
	}
	if v, ok := snap.State["desktop"].(DesktopChanged); ok {
		out.Desktop = &v
	}
	return out, nil
}

// Leave takes the device out of the screen's room.
func (m *Manager) Leave(d device.ID, id string) { m.events.Leave(d, room(id)) }

// Boot starts a device; see Boot.
func (m *Manager) Boot(ctx context.Context, id string) error { return Boot(ctx, id) }

// Shutdown stops a device and, once it has stopped, forgets its controller.
func (m *Manager) Shutdown(ctx context.Context, id string) error {
	err := Shutdown(ctx, id)
	if err == nil {
		m.forget(id)
	}
	return err
}

// controller carries out a screen's actions, turning each into the
// platform's command, keeps the settings it reports, and holds its open
// live views.
type controller struct {
	d   Screen
	m   *Manager
	adb string

	// publishMu orders the view's pushes, taken before mu: a push is a
	// socket write, which mustn't hold up the frames reading mu.
	publishMu sync.Mutex
	mu        sync.Mutex
	state     State
	// orientation is the UIDeviceOrientation the host last turned it to:
	// 1 portrait, 2 upside down, 3 landscape left, 4 landscape right.
	orientation int
	// rotation is how far, clockwise, the framebuffer turns to show the
	// screen upright: 0, 90, 180 or -90. Each view's simcap turns its frames
	// by it before encoding, so they go out upright and picture in picture
	// on the phone takes the screen's shape. A rotate sets it as it starts:
	// the screen starts turning before the device answers.
	rotation int
	// foldable is set when simcap reports two panels (iOS) or the AVD has a
	// hinge (Android).
	foldable bool
	// displayed is set once simcap reports the panel's turn, which then
	// decides rotation rather than the orientation the host asked for.
	displayed bool
	// activeScreen is the panel simcap shows, by the screen ID simctl's
	// --display takes; 0 until it says.
	activeScreen uint32
	// views are the open live views, newest last: an action that needs the
	// device's input path takes the newest.
	views []*liveView
	// commands are simcap actions waiting for it to say they're done.
	nextCommand int
	commands    map[int]chan commandDone

	// agent is what an agent or the CLI drives the screen with: see agent.go.
	agent agentState

	// desktopMu orders desktop pushes; lastDesktop is the last one sent.
	desktopMu   sync.Mutex
	lastDesktop *DesktopView

	// restart asks an Android recorder to start again, and sizeChanged its
	// touches to read the screen again: a rotation or fold changed its size.
	restart     chan struct{}
	sizeChanged atomic.Bool
	// refresh asks a Mac screen's desktop to be read again, after an action.
	refresh chan struct{}
}

// liveView is one open /stream: forward writes to its device's input path.
type liveView struct {
	forward func(capture.Input) error
}

func newController(d Screen, m *Manager) *controller {
	c := &controller{d: d, m: m, orientation: 1, commands: map[int]chan commandDone{},
		restart: make(chan struct{}, 1), refresh: make(chan struct{}, 1)}
	if d.Platform == PlatformAndroid {
		c.adb = androidTool("platform-tools", "adb")
		c.foldable = hasHinge(d.ID)
	}
	return c
}

// read reads the device's settings, then sends the view.
func (c *controller) read(ctx context.Context) {
	var s State
	switch c.d.Platform {
	case PlatformIOS:
		s = c.readIOS(ctx)
	case PlatformAndroid:
		s = c.readAndroid(ctx)
	default:
		c.publish()
		return
	}
	c.update(func(cur *State) {
		// What the host set and can't read back stays.
		s.StatusBar, s.SlowAnimations = cur.StatusBar, cur.SlowAnimations
		if c.d.Platform == PlatformIOS {
			s.OnscreenKeyboard, s.Pose = cur.OnscreenKeyboard, cur.Pose
		}
		*cur = s
	})
}

// controls are the actions this screen takes.
func (c *controller) controls() []Control {
	if c.d.Kind == KindDisplay {
		return []Control{ControlText, ControlDesktop}
	}
	out := []Control{ControlAppearance, ControlContentSize, ControlContrast, ControlStatusBar,
		ControlSlowAnimations, ControlRotate, ControlOpenURL, ControlLocation, ControlScreenshot}
	if c.foldable {
		out = append(out, ControlPose)
	}
	if c.d.Platform == PlatformAndroid {
		return append(out, ControlText)
	}
	out = append(out, ControlPush, ControlShake, ControlMemoryWarning, ControlOnscreenKeyboard)
	if !c.universalHID() || capture.UniversalHID {
		out = append(out, ControlText)
	}
	return out
}

// buttons are the hardware buttons this screen takes over /stream.
func (c *controller) buttons() []HardwareButton {
	switch {
	case c.d.Kind == KindDisplay:
		return []HardwareButton{}
	case c.d.Platform == PlatformAndroid:
		return []HardwareButton{ButtonHome, ButtonBack, ButtonRecents, ButtonLock, ButtonVolumeUp, ButtonVolumeDown}
	case !c.universalHID():
		return []HardwareButton{ButtonHome, ButtonAppSwitcher, ButtonLock, ButtonSiri, ButtonVolumeUp, ButtonVolumeDown}
	}
	// iOS 27 takes its buttons as Indigo messages, which it ignores.
	return []HardwareButton{ButtonHome, ButtonAppSwitcher}
}

// supports refuses an action this screen doesn't take.
func (c *controller) supports(control Control) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !slices.Contains(c.controls(), control) {
		return ErrUnsupported
	}
	return nil
}

// update changes the state and sends the view: snapshots go out in the
// order the changes were made.
func (c *controller) update(change func(*State)) { c.change(func() { change(&c.state) }) }

func (c *controller) publish() { c.change(func() {}) }

// change runs fn under mu, then sends the view it left.
func (c *controller) change(fn func()) {
	c.publishMu.Lock()
	defer c.publishMu.Unlock()
	c.mu.Lock()
	fn()
	view := ViewChanged{Screen: c.d.ID, Controls: c.controls(), Buttons: c.buttons()}
	if c.d.Kind != KindDisplay {
		state := c.state
		view.State = &state
	}
	c.mu.Unlock()
	c.m.events.Publish(room(c.d.ID), view, nil)
}

func (c *controller) streaming() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.views) > 0
}

// publishDesktop sends the Mac's desktop when it changed.
func (c *controller) publishDesktop(v DesktopView) {
	c.desktopMu.Lock()
	defer c.desktopMu.Unlock()
	if c.lastDesktop != nil && sameJSON(*c.lastDesktop, v) {
		return
	}
	c.lastDesktop = &v
	c.m.events.Publish(room(c.d.ID), DesktopChanged{Screen: c.d.ID, Desktop: v}, nil)
}

func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// open adds a live view whose input path is forward; close takes it away.
func (c *controller) open(forward func(capture.Input) error) (close func()) {
	v := &liveView{forward: forward}
	c.mu.Lock()
	c.views = append(c.views, v)
	turn := c.turnInput()
	c.mu.Unlock()
	if c.d.Platform == PlatformIOS && c.d.Kind == KindSimulator {
		forward(turn)
	}
	return func() {
		c.mu.Lock()
		c.views = slices.DeleteFunc(c.views, func(x *liveView) bool { return x == v })
		c.mu.Unlock()
	}
}

// forward writes to the newest live view's input path.
func (c *controller) forward(in capture.Input) error {
	c.mu.Lock()
	var v *liveView
	if n := len(c.views); n > 0 {
		v = c.views[n-1]
	}
	c.mu.Unlock()
	if v == nil {
		return ErrNotStreaming
	}
	return v.forward(in)
}

func ptr[T any](v T) *T { return &v }

// turnInput tells a simcap the rotation; c.mu held.
func (c *controller) turnInput() capture.Input {
	return capture.Input{Action: capture.ActionTurn, Value: strconv.Itoa(((c.rotation/90)%4 + 4) % 4)}
}

// showTurn has every open view turn its frames to the rotation and send one
// now: a still screen (the Home screen, which doesn't turn with the device)
// sends none of its own, so it would keep its unturned shape until
// something moved.
func (c *controller) showTurn() {
	c.mu.Lock()
	views := slices.Clone(c.views)
	turn := c.turnInput()
	c.mu.Unlock()
	for _, v := range views {
		v.forward(turn)
		v.forward(capture.Input{Action: capture.ActionKeyframe})
	}
}

// commandWait is how long simcap may take to carry out an action.
const commandWait = 10 * time.Second

// command sends an action to simcap and waits for it to say it's done.
func (c *controller) command(ctx context.Context, in capture.Input) error {
	_, err := c.commandVia(ctx, c.forward, in)
	return err
}

// commandDone is simcap's answer to an action: its error, and its data.
type commandDone struct {
	err  error
	data json.RawMessage
}

// commandVia sends an action to simcap through one input path and waits for
// its answer.
func (c *controller) commandVia(ctx context.Context, path func(capture.Input) error, in capture.Input) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextCommand++
	in.ID = c.nextCommand
	done := make(chan commandDone, 1)
	c.commands[in.ID] = done
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.commands, in.ID)
		c.mu.Unlock()
	}()
	if err := path(in); err != nil {
		return nil, err
	}
	select {
	case d := <-done:
		return d.data, d.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(commandWait):
		return nil, errors.New("the simulator didn't answer")
	}
}

// fromSimcap takes a message simcap sent the host: the active panel and its
// turn, or that an action finished.
func (c *controller) fromSimcap(message []byte) {
	var m struct {
		Display *struct {
			Screen   uint32 `json:"screen"`
			Rotation int    `json:"rotation"`
			Foldable bool   `json:"foldable"`
		} `json:"display"`
		Done *struct {
			ID    int             `json:"id"`
			Error string          `json:"error"`
			Data  json.RawMessage `json:"data"`
		} `json:"done"`
	}
	if json.Unmarshal(message, &m) != nil {
		return
	}
	if d := m.Done; d != nil {
		c.mu.Lock()
		done := c.commands[d.ID]
		c.mu.Unlock()
		if done != nil {
			var err error
			if d.Error != "" {
				err = errors.New(d.Error)
			}
			done <- commandDone{err: err, data: d.Data}
		}
		return
	}
	if m.Display == nil {
		return
	}
	turned := false
	c.change(func() {
		if c.activeScreen != m.Display.Screen || c.rotation != upright(m.Display.Rotation) {
			// A fold or turn moved everything: the last read's refs are gone.
			c.agent.read = nil
		}
		c.foldable = c.foldable || m.Display.Foldable
		c.displayed = true
		c.activeScreen = m.Display.Screen
		turned = c.rotation != upright(m.Display.Rotation)
		c.rotation = upright(m.Display.Rotation)
		// The device's real turn, so the next rotate starts from it: one the
		// host assumed (portrait, for a device it just met) sent a turn the
		// device was already at, and nothing moved.
		c.orientation = orientationAt(m.Display.Rotation)
	})
	if turned {
		// Off simcap's reader, which this is: the write goes to its input.
		go c.showTurn()
	}
}

func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// shellQuote quotes s for the device's sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
