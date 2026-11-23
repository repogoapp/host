package screens

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/repogoapp/host/internal/screens/capture"
)

// Driving a simulator or emulator by what's on its screen, for agents and
// `repogo sim`: read the screen by label, tap, swipe, type, press keys and
// buttons. Each action is built as a phone's StreamInput and run through
// controller.translate, so it behaves as the same input from a live view.

// Element is one thing on the screen, in the device's units (Elements.Unit)
// with the screen turned as it shows.
type Element struct {
	// Ref names it for the next action: e7.12 is element 12 of read 7.
	Ref      string  `json:"ref"`
	Role     string  `json:"role"`
	Label    string  `json:"label"`
	Value    string  `json:"value,omitempty"`
	ID       string  `json:"id,omitempty"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	W        float64 `json:"w"`
	H        float64 `json:"h"`
	Selected bool    `json:"selected,omitempty"`
}

// Elements is one read of a screen.
type Elements struct {
	Screen string  `json:"screen"`
	App    string  `json:"app,omitempty"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
	// Unit is pt on iOS and px on Android: what taps and swipes take too.
	Unit     string    `json:"unit"`
	Elements []Element `json:"elements" wire:"array"`
}

// Point is a place on the screen, in the device's units.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Target is what a tap aims at: a ref from the last read, a label, or a point.
type Target struct {
	Ref   string   `json:"ref,omitempty"`
	Label string   `json:"label,omitempty"`
	X     *float64 `json:"x,omitempty"`
	Y     *float64 `json:"y,omitempty"`
}

// agentState is a controller's part for agents.
type agentState struct {
	// lock runs one action at a time on the screen, so a tap's touches all
	// go one way and a read's refs number the screen it read.
	lock sync.Mutex
	// read is the last read, and reads how many there have been; c.mu.
	read  *Elements
	reads int
	// generation counts the screen's changes (an action, a fold or turn, the
	// phone's input): a read is kept only if none came while it ran; c.mu.
	generation int
	// closed is set when the screen shut down: nothing new starts; c.mu.
	closed bool
	// headless is the input path started for agents when no live view was
	// open (iOS); c.mu.
	headless *headlessView
	// android reads an emulator's screen; lock.
	android *axHelper
	// recording is the screen's recording while it runs; c.mu.
	recording *recording
}

// headlessView is a simcap child with no frames, kept while agents use it.
type headlessView struct {
	cancel context.CancelFunc
	used   atomic.Int64
}

// headlessIdle is how long an unused headless view stays.
const headlessIdle = 2 * time.Minute

// agentController is the screen's controller, for a simulator or emulator.
func (m *Manager) agentController(ctx context.Context, id string) (*controller, error) {
	c, err := m.controller(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.d.Kind == KindDisplay {
		return nil, errInvalid.Errorf("%s is this Mac's display: only simulators and emulators take these", id)
	}
	return c, nil
}

// screenChanged forgets the last read: the screen may no longer be it.
func (c *controller) screenChanged() {
	c.mu.Lock()
	c.screenChangedLocked()
	c.mu.Unlock()
}

// screenChangedLocked is screenChanged with c.mu held.
func (c *controller) screenChangedLocked() {
	c.agent.generation++
	c.agent.read = nil
}

// Elements reads what's on the screen now, numbering each element for the
// next actions.
func (m *Manager) Elements(ctx context.Context, id string) (Elements, error) {
	c, err := m.agentController(ctx, id)
	if err != nil {
		return Elements{}, err
	}
	c.agent.lock.Lock()
	defer c.agent.lock.Unlock()
	return c.readElements(ctx)
}

// Tap taps the target's center, answering where.
func (m *Manager) Tap(ctx context.Context, id string, t Target) (Point, error) {
	c, err := m.agentController(ctx, id)
	if err != nil {
		return Point{}, err
	}
	c.agent.lock.Lock()
	defer c.agent.lock.Unlock()
	at, err := c.aim(ctx, t)
	if err != nil {
		return Point{}, err
	}
	return at, c.gesture(ctx, []Point{at}, 0)
}

// Swipe drags one finger from one point to another over ms milliseconds.
func (m *Manager) Swipe(ctx context.Context, id string, from, to Point, ms int) error {
	if ms <= 0 {
		ms = 300
	}
	if ms > 5000 {
		return errInvalid.Errorf("a swipe takes at most 5000 ms")
	}
	c, err := m.agentController(ctx, id)
	if err != nil {
		return err
	}
	c.agent.lock.Lock()
	defer c.agent.lock.Unlock()
	return c.gesture(ctx, []Point{from, to}, ms)
}

// Type types text into the focused field.
func (m *Manager) Type(ctx context.Context, id, text string) error {
	if text == "" {
		return errInvalid.Errorf("no text to type")
	}
	return m.press(ctx, id, StreamInput{Text: &TextInput{Text: text}})
}

// keysyms are the keys an agent presses by name.
var keysyms = map[string]int{
	"return": 0xff0d, "enter": 0xff0d, "delete": 0xff08, "backspace": 0xff08, "forward_delete": 0xffff,
	"tab": 0xff09, "escape": 0xff1b, "space": 0x20,
	"left": 0xff51, "up": 0xff52, "right": 0xff53, "down": 0xff54,
}

// KeyNames are the keys Key presses.
func KeyNames() []string {
	names := make([]string, 0, len(keysyms))
	for name := range keysyms {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Key presses and releases one key by name (KeyNames).
func (m *Manager) Key(ctx context.Context, id, name string) error {
	sym, ok := keysyms[strings.ToLower(name)]
	if !ok {
		return errInvalid.Errorf("unknown key %q: one of %s", name, strings.Join(KeyNames(), ", "))
	}
	return m.press(ctx, id,
		StreamInput{Key: &KeyInput{Keysym: sym, Down: true}},
		StreamInput{Key: &KeyInput{Keysym: sym}})
}

// Button presses a hardware button the screen takes.
func (m *Manager) Button(ctx context.Context, id string, b HardwareButton) error {
	c, err := m.agentController(ctx, id)
	if err != nil {
		return err
	}
	c.mu.Lock()
	takes := c.buttons()
	c.mu.Unlock()
	if !slices.Contains(takes, b) {
		names := make([]string, len(takes))
		for i, t := range takes {
			names[i] = string(t)
		}
		return errInvalid.Errorf("this screen takes %s, not %q", strings.Join(names, ", "), b)
	}
	return m.press(ctx, id, StreamInput{Button: &ButtonInput{Button: b}})
}

// press runs inputs as a live view would, through one input path.
func (m *Manager) press(ctx context.Context, id string, inputs ...StreamInput) error {
	c, err := m.agentController(ctx, id)
	if err != nil {
		return err
	}
	c.agent.lock.Lock()
	defer c.agent.lock.Unlock()
	path, err := c.inputPath(ctx)
	if err != nil {
		return err
	}
	for _, in := range inputs {
		items := c.translate(in)
		if len(items) == 0 {
			return ErrUnsupported
		}
		for _, it := range items {
			if it.host != nil {
				err = it.host(ctx, path)
			} else {
				err = path(it.device)
			}
			if err != nil {
				return err
			}
		}
	}
	c.screenChanged()
	return nil
}

// Launch opens an app by its bundle ID (iOS) or package (Android), or
// brings it to the front.
func (m *Manager) Launch(ctx context.Context, id, app string) error {
	if !appName.MatchString(app) {
		return errInvalid.Errorf("not an app id: %q", app)
	}
	c, err := m.agentController(ctx, id)
	if err != nil {
		return err
	}
	if c.d.Platform == PlatformIOS {
		_, err = c.simctl(ctx, "launch", c.d.ID, app)
	} else {
		_, err = c.shell(ctx, "monkey -p "+app+" -c android.intent.category.LAUNCHER 1")
	}
	c.screenChanged()
	return err
}

// appName is a bundle ID or package: letters, digits, dots, dashes, underscores.
var appName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// SaveScreenshot writes the screen as a PNG under the host's folder, a new
// file each time, and answers its path.
func (m *Manager) SaveScreenshot(ctx context.Context, id string) (string, error) {
	c, err := m.agentController(ctx, id)
	if err != nil {
		return "", err
	}
	var shot []byte
	if c.d.Platform == PlatformIOS {
		if err := c.knowPanel(ctx); err != nil {
			return "", err
		}
		shot, err = c.screenshotIOSAs(ctx, "png")
	} else {
		shot, err = c.screencapAndroid(ctx)
	}
	if err != nil {
		return "", err
	}
	path, err := newScreenFile(id, ".png")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, shot, 0o600)
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// readElements reads the screen and keeps it as the last read; c.agent.lock held.
func (c *controller) readElements(ctx context.Context) (Elements, error) {
	var e Elements
	var err error
	c.mu.Lock()
	generation := c.agent.generation
	c.mu.Unlock()
	if c.d.Platform == PlatformIOS {
		e, err = c.readIOSElements(ctx)
	} else {
		e, err = c.readAndroidElements(ctx)
	}
	if err != nil {
		return Elements{}, err
	}
	e.Screen = c.d.ID
	if e.Elements == nil {
		e.Elements = []Element{}
	}
	c.mu.Lock()
	c.agent.reads++
	for i := range e.Elements {
		e.Elements[i].Ref = fmt.Sprintf("e%d.%d", c.agent.reads, i+1)
	}
	// A read the screen changed under isn't kept: its refs could tap the wrong thing.
	if c.agent.generation == generation {
		kept := e
		c.agent.read = &kept
	}
	c.mu.Unlock()
	return e, nil
}

func (c *controller) readIOSElements(ctx context.Context) (Elements, error) {
	path, err := c.inputPath(ctx)
	if err != nil {
		return Elements{}, err
	}
	data, err := c.commandVia(ctx, path, capture.Input{Action: capture.ActionElements})
	if err != nil {
		return Elements{}, err
	}
	var answer struct {
		Elements
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &answer); err != nil {
		return Elements{}, fmt.Errorf("the simulator's screen read wrong: %w", err)
	}
	if answer.Error != "" {
		return Elements{}, errors.New(answer.Error)
	}
	answer.Unit = "pt"
	for i := range answer.Elements.Elements {
		el := &answer.Elements.Elements[i]
		// A tab or radio button reports being picked as its value.
		if (el.Role == "RadioButton" || el.Role == "Tab") && el.Value == "1" {
			el.Selected, el.Value = true, ""
		} else if el.Role == "RadioButton" || el.Role == "Tab" {
			el.Value = ""
		}
	}
	return answer.Elements, nil
}

// aim is where a target is, reading the screen when it names a label;
// c.agent.lock held.
func (c *controller) aim(ctx context.Context, t Target) (Point, error) {
	given := 0
	for _, set := range []bool{t.Ref != "", t.Label != "", t.X != nil || t.Y != nil} {
		if set {
			given++
		}
	}
	if given != 1 || (t.X == nil) != (t.Y == nil) {
		return Point{}, errInvalid.Errorf("tap takes one of ref, label, or x and y")
	}
	if t.X != nil {
		return Point{X: *t.X, Y: *t.Y}, nil
	}
	var el Element
	if t.Ref != "" {
		c.mu.Lock()
		read := c.agent.read
		c.mu.Unlock()
		found := false
		if read != nil {
			for _, e := range read.Elements {
				if e.Ref == t.Ref {
					el, found = e, true
					break
				}
			}
		}
		if !found {
			return Point{}, errInvalid.Errorf("%s isn't on the last read of the screen: read the screen again", t.Ref)
		}
	} else {
		read, err := c.readElements(ctx)
		if err != nil {
			return Point{}, err
		}
		if el, err = match(read.Elements, t.Label); err != nil {
			return Point{}, err
		}
	}
	return Point{X: el.X + el.W/2, Y: el.Y + el.H/2}, nil
}

// match finds the one element a label names: by its exact label, then its
// exact id, then the one whose label contains it, then the unlabeled field
// whose text or prompt does.
func match(elements []Element, label string) (Element, error) {
	contains := func(s string) bool { return strings.Contains(strings.ToLower(s), strings.ToLower(label)) }
	tries := []func(Element) bool{
		func(e Element) bool { return e.Label == label },
		func(e Element) bool { return e.ID == label },
		func(e Element) bool { return contains(e.Label) },
		func(e Element) bool { return e.Label == "" && strings.Contains(e.Role, "Field") && contains(e.Value) },
	}
	for _, try := range tries {
		var found []Element
		for _, e := range elements {
			if try(e) {
				found = append(found, e)
			}
		}
		switch {
		case len(found) == 1:
			return found[0], nil
		case len(found) > 1:
			names := make([]string, 0, len(found))
			for _, e := range found {
				names = append(names, fmt.Sprintf("%s %s %q", e.Ref, e.Role, e.Label))
			}
			return Element{}, errInvalid.Errorf("%d things match %q; tap one by ref: %s", len(found), label,
				strings.Join(names, ", "))
		}
	}
	return Element{}, errInvalid.Errorf("nothing on the screen is labeled %q: read the screen to see what is", label)
}

// gesture touches down at the first point, moves through the rest over ms,
// and lifts; c.agent.lock held.
func (c *controller) gesture(ctx context.Context, points []Point, ms int) error {
	if c.d.Platform == PlatformAndroid {
		return c.androidGesture(ctx, points, ms)
	}
	size, err := c.screenSize(ctx)
	if err != nil {
		return err
	}
	path, err := c.inputPath(ctx)
	if err != nil {
		return err
	}
	// Every point is checked before the finger goes down, so none is left down.
	for _, p := range points {
		if x, y := p.X/size.X, p.Y/size.Y; x < 0 || x > 1 || y < 0 || y > 1 {
			return errInvalid.Errorf("%.0f, %.0f is off the %.0f×%.0f screen", p.X, p.Y, size.X, size.Y)
		}
	}
	at := points[0]
	touch := func(phase TouchPhase, p Point) error {
		at = p
		x, y := p.X/size.X, p.Y/size.Y
		for _, it := range c.translate(StreamInput{Touch: &TouchInput{Phase: phase, X: x, Y: y}}) {
			if err := path(it.device); err != nil {
				return err
			}
		}
		return nil
	}
	if err := touch(TouchDown, points[0]); err != nil {
		return err
	}
	if len(points) == 1 {
		time.Sleep(60 * time.Millisecond)
	} else {
		from, to := points[0], points[len(points)-1]
		steps := max(ms/16, 2)
		for i := 1; i <= steps; i++ {
			time.Sleep(time.Duration(ms/steps) * time.Millisecond)
			f := float64(i) / float64(steps)
			if err := touch(TouchMove, Point{X: from.X + (to.X-from.X)*f, Y: from.Y + (to.Y-from.Y)*f}); err != nil {
				touch(TouchUp, at)
				return err
			}
		}
	}
	err = touch(TouchUp, points[len(points)-1])
	c.screenChanged()
	return err
}

// screenSize is the screen's width and height in the device's units, from
// the last read, or a new one; c.agent.lock held.
func (c *controller) screenSize(ctx context.Context) (Point, error) {
	c.mu.Lock()
	read := c.agent.read
	c.mu.Unlock()
	if read == nil {
		r, err := c.readElements(ctx)
		if err != nil {
			return Point{}, err
		}
		read = &r
	}
	if read.Width < 1 || read.Height < 1 {
		return Point{}, errors.New("the screen's size isn't known: is an app open?")
	}
	return Point{X: read.Width, Y: read.Height}, nil
}

// inputPath is where the device's input goes for one action: on iOS the
// newest live view's simcap, or a headless one started for agents; on
// Android the emulator's `input` command.
func (c *controller) inputPath(ctx context.Context) (func(capture.Input) error, error) {
	if c.d.Platform == PlatformAndroid {
		return c.androidPath(ctx), nil
	}
	c.mu.Lock()
	if c.agent.closed {
		c.mu.Unlock()
		return nil, ErrNotRunning
	}
	h := c.agent.headless
	open := len(c.views)
	if open == 0 && h == nil {
		h = c.startHeadless()
	}
	c.mu.Unlock()
	if h != nil {
		h.used.Store(time.Now().UnixNano())
	}
	// The child takes a moment to start, then to say which panel shows
	// (iOS 27): touches before that may go to the wrong one.
	started := time.Now()
	for {
		c.mu.Lock()
		var v *liveView
		if n := len(c.views); n > 0 {
			v = c.views[n-1]
		}
		displayed := c.displayed || !c.universalHID()
		c.mu.Unlock()
		if v != nil && (displayed || time.Since(started) > 2*time.Second) {
			return v.forward, nil
		}
		if time.Since(started) > 8*time.Second {
			return nil, errors.New("couldn't reach the simulator's input")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// startHeadless starts a simcap child with no frames for agents' input; c.mu held.
func (c *controller) startHeadless() *headlessView {
	ctx, cancel := context.WithCancel(context.Background())
	h := &headlessView{cancel: cancel}
	h.used.Store(time.Now().UnixNano())
	c.agent.headless = h
	args := []string{"simcap", c.d.ID}
	if c.universalHID() {
		args = append(args, "universal")
	}
	args = append(args, "headless")
	go func() {
		err := streamChild(ctx, c, func([]byte) error { return nil }, nil, newInputQueue(), args, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "screens: headless input:", err)
		}
		cancel()
		c.mu.Lock()
		if c.agent.headless == h {
			c.agent.headless = nil
		}
		c.mu.Unlock()
	}()
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if time.Since(time.Unix(0, h.used.Load())) > headlessIdle {
					cancel()
					return
				}
			}
		}
	}()
	return h
}

// closeAgent stops what agents started on the screen: it shut down. It
// waits for an action under way, so what that starts is stopped too, and
// nothing starts after.
func (c *controller) closeAgent() {
	c.agent.lock.Lock()
	defer c.agent.lock.Unlock()
	c.mu.Lock()
	c.agent.closed = true
	h, r := c.agent.headless, c.agent.recording
	c.agent.headless, c.agent.recording = nil, nil
	c.screenChangedLocked()
	c.mu.Unlock()
	if r != nil {
		r.stop()
	}
	if h != nil {
		h.cancel()
	}
	if c.agent.android != nil {
		c.agent.android.close()
		c.agent.android = nil
	}
}

// androidPath runs the device input as the emulator's `input` command.
func (c *controller) androidPath(ctx context.Context) func(capture.Input) error {
	w, h := c.screen(ctx)
	return func(in capture.Input) error {
		var lines []string
		switch {
		case in.Action == capture.ActionText:
			lines = androidText(in.Value)
		case in.Key != nil:
			if !in.Key.Down {
				return nil
			}
			if line := androidKey(in.Key.Keysym); line != "" {
				lines = []string{line}
			}
		default:
			if line := androidCommand(in, w, h); line != "" {
				lines = []string{line}
			}
		}
		if len(lines) == 0 {
			return ErrUnsupported
		}
		_, err := c.shell(ctx, strings.Join(lines, "; "))
		return err
	}
}

// androidGesture is a tap or a swipe as one `input` command, in pixels.
func (c *controller) androidGesture(ctx context.Context, points []Point, ms int) error {
	w, h := c.screen(ctx)
	px := func(p Point) (int, int, error) {
		x, y := int(math.Round(p.X)), int(math.Round(p.Y))
		if x < 0 || y < 0 || (w > 0 && x >= w) || (h > 0 && y >= h) {
			return 0, 0, errInvalid.Errorf("%d, %d is off the %d×%d screen", x, y, w, h)
		}
		return x, y, nil
	}
	x1, y1, err := px(points[0])
	if err != nil {
		return err
	}
	command := fmt.Sprintf("input tap %d %d", x1, y1)
	if len(points) > 1 {
		x2, y2, err := px(points[len(points)-1])
		if err != nil {
			return err
		}
		command = fmt.Sprintf("input swipe %d %d %d %d %d", x1, y1, x2, y2, ms)
	}
	_, err = c.shell(ctx, command)
	c.screenChanged()
	return err
}
