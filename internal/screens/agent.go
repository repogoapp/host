package screens

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Driving a simulator or emulator by what's on its screen, for agents and
// `repogo screens`: read the screen by label, tap, swipe, type, press keys
// and buttons. This file holds the types and actions; read.go and wait.go
// read the screen, input.go and androidax.go reach the device, record.go and
// screenfiles.go save what it shows.

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
	// Checked is a switch's or checkbox's state, where it has one.
	Checked  *bool `json:"checked,omitempty"`
	Disabled bool  `json:"disabled,omitempty"`
	Focused  bool  `json:"focused,omitempty"`
	// Secure is a password field: screenshots for agents black it out.
	Secure bool `json:"secure,omitempty"`
	// Covered is under the on-screen keyboard: a tap there would hit a key.
	Covered bool `json:"covered,omitempty"`
}

// Line is the element on one line, as the CLI prints it and agents read it:
// e3.12 Button "Files" selected #doc.text @881,585 44x58
func (e Element) Line() string {
	num := func(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %q", e.Ref, e.Role, e.Label)
	if e.Value != "" {
		fmt.Fprintf(&b, " =%q", e.Value)
	}
	for _, state := range []struct {
		on   bool
		word string
	}{
		{e.Selected, "selected"}, {e.Checked != nil && *e.Checked, "on"}, {e.Checked != nil && !*e.Checked, "off"},
		{e.Disabled, "disabled"}, {e.Focused, "focused"}, {e.Secure, "secure"}, {e.Covered, "covered"},
	} {
		if state.on {
			b.WriteString(" " + state.word)
		}
	}
	if e.ID != "" && e.ID != e.Label {
		b.WriteString(" #" + e.ID)
	}
	fmt.Fprintf(&b, " @%s,%s %sx%s", num(e.X), num(e.Y), num(e.W), num(e.H))
	return b.String()
}

// Elements is one read of a screen.
type Elements struct {
	Screen string  `json:"screen"`
	App    string  `json:"app,omitempty"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
	// Unit is pt on iOS and px on Android: what taps and swipes take too.
	Unit string `json:"unit"`
	// Keyboard is set while the on-screen keyboard is up, its keys left out
	// of Elements; screens_key hide_keyboard sends it away.
	Keyboard bool `json:"keyboard,omitempty"`
	// keyboardTop is where the keyboard starts, when its frame is known.
	keyboardTop float64
	// pid is the front app's process (iOS), for Launch to wait for its own.
	pid int
	// Pose is a foldable's, as its lit panel shows it.
	Pose     *Pose     `json:"pose,omitempty"`
	Elements []Element `json:"elements" wire:"array"`
	// Changes is set on an action's answer that lists in Elements only what's
	// new or changed since the screen before it: refs not listed still hold,
	// Gone lists those no longer there, and Unchanged counts the rest.
	Changes   bool     `json:"changes,omitempty"`
	Gone      []string `json:"gone,omitempty"`
	Unchanged int      `json:"unchanged,omitempty"`
}

// find is the element ref names in this read; a nil read has none.
func (e *Elements) find(ref string) (Element, bool) {
	if e == nil {
		return Element{}, false
	}
	for _, el := range e.Elements {
		if el.Ref == ref {
			return el, true
		}
	}
	return Element{}, false
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
	// stale is the read before the screen last changed: a ref from it is
	// found again by what it named, where that's unambiguous; c.mu.
	stale *Elements
	// generation counts the screen's changes (an action, a fold or turn, the
	// phone's input): a read is kept only if none came while it ran; c.mu.
	generation int
	// closed is set when the screen shut down: nothing new starts; c.mu.
	closed bool
	// movedAt is when a fold or turn was last asked for; c.mu.
	movedAt time.Time
	// headless is the input path started for agents when no live view was
	// open (iOS); c.mu.
	headless *headlessView
	// android reads an emulator's screen; lock.
	android *axHelper
	// recording is the screen's recording while it runs; c.mu.
	recording *recording
	// poseReadAt is when devicectl was last asked for the pose; c.mu.
	poseReadAt time.Time
	// phoneAt is when a phone's live view last touched or typed; c.mu.
	phoneAt time.Time
}

// phoneHold is how long after a phone's last input agents' input waits: the
// person driving the screen from RepoGo keeps it while they use it.
const phoneHold = 5 * time.Second

// phoneUsed notes the phone's input on the screen, for phoneHold.
func (c *controller) phoneUsed() {
	c.mu.Lock()
	c.agent.phoneAt = time.Now()
	c.mu.Unlock()
}

// held refuses an agent's input while someone uses the screen from the phone.
func (c *controller) held() error {
	c.mu.Lock()
	left := phoneHold - time.Since(c.agent.phoneAt)
	c.mu.Unlock()
	if left <= 0 {
		return nil
	}
	return errInvalid.Errorf("someone is using this screen from the RepoGo app: try again in %d s", int(left.Seconds())+1)
}

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

// lockAgent is the screen's controller with its agent lock held, for one
// action; the caller defers unlock.
func (m *Manager) lockAgent(ctx context.Context, id string) (c *controller, unlock func(), err error) {
	c, err = m.agentController(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	c.agent.lock.Lock()
	return c, c.agent.lock.Unlock, nil
}

// lockInput is lockAgent for an action that touches, types or presses,
// refused while someone uses the screen from the phone.
func (m *Manager) lockInput(ctx context.Context, id string) (c *controller, unlock func(), err error) {
	c, unlock, err = m.lockAgent(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if err := c.held(); err != nil {
		unlock()
		return nil, nil, err
	}
	return c, unlock, nil
}

// exactlyOne is whether exactly one of set is true.
func exactlyOne(set ...bool) bool {
	n := 0
	for _, s := range set {
		if s {
			n++
		}
	}
	return n == 1
}

// Elements reads what's on the screen now, numbering each element for the
// next actions.
func (m *Manager) Elements(ctx context.Context, id string) (Elements, error) {
	c, unlock, err := m.lockAgent(ctx, id)
	if err != nil {
		return Elements{}, err
	}
	defer unlock()
	return c.readElements(ctx)
}

// Tap taps the target's center, answering where.
func (m *Manager) Tap(ctx context.Context, id string, t Target) (Point, error) {
	c, unlock, err := m.lockInput(ctx, id)
	if err != nil {
		return Point{}, err
	}
	defer unlock()
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
	c, unlock, err := m.lockInput(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	return c.gesture(ctx, []Point{from, to}, ms)
}

// Type types text into the focused field.
func (m *Manager) Type(ctx context.Context, id, text string) error {
	if text == "" {
		return errInvalid.Errorf("no text to type")
	}
	return m.press(ctx, id, StreamInput{Text: &TextInput{Text: text}})
}

// press runs inputs on the screen, one action under its agent lock.
func (m *Manager) press(ctx context.Context, id string, inputs ...StreamInput) error {
	c, unlock, err := m.lockInput(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	return c.press(ctx, inputs...)
}

// keysyms are the keys an agent presses by name.
var keysyms = map[string]int{
	"return": 0xff0d, "enter": 0xff0d, "delete": 0xff08, "backspace": 0xff08, "forward_delete": 0xffff,
	"tab": 0xff09, "escape": 0xff1b, "space": 0x20,
	"left": 0xff51, "up": 0xff52, "right": 0xff53, "down": 0xff54,
}

// hideKeyboard is the key that sends the on-screen keyboard away.
const hideKeyboard = "hide_keyboard"

// KeyNames are the keys Key presses.
func KeyNames() []string {
	names := make([]string, 0, len(keysyms)+1)
	for name := range keysyms {
		names = append(names, name)
	}
	names = append(names, hideKeyboard)
	slices.Sort(names)
	return names
}

// Key presses and releases one key by name (KeyNames).
func (m *Manager) Key(ctx context.Context, id, name string) error {
	if strings.EqualFold(name, hideKeyboard) {
		return m.HideKeyboard(ctx, id)
	}
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
	c, unlock, err := m.lockInput(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	c.mu.Lock()
	supported := c.buttons()
	c.mu.Unlock()
	if !slices.Contains(supported, b) {
		names := make([]string, len(supported))
		for i, s := range supported {
			names[i] = string(s)
		}
		return errInvalid.Errorf("this screen takes %s, not %q", strings.Join(names, ", "), b)
	}
	return c.press(ctx, StreamInput{Button: &ButtonInput{Button: b}})
}

// grants are the permissions Launch grants by name: simctl privacy's
// services on iOS, the runtime permissions on Android.
var grants = map[string]struct {
	ios     string
	android []string
}{
	"calendar":      {"calendar", []string{"READ_CALENDAR", "WRITE_CALENDAR"}},
	"camera":        {"", []string{"CAMERA"}},
	"contacts":      {"contacts", []string{"READ_CONTACTS", "WRITE_CONTACTS"}},
	"location":      {"location", []string{"ACCESS_FINE_LOCATION", "ACCESS_COARSE_LOCATION"}},
	"microphone":    {"microphone", []string{"RECORD_AUDIO"}},
	"motion":        {"motion", []string{"ACTIVITY_RECOGNITION"}},
	"notifications": {"", []string{"POST_NOTIFICATIONS"}},
	"photos":        {"photos", []string{"READ_MEDIA_IMAGES", "READ_MEDIA_VIDEO"}},
	"reminders":     {"reminders", nil},
}

// GrantNames are the permissions Launch grants.
func GrantNames() []string {
	names := make([]string, 0, len(grants))
	for name := range grants {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// Launch opens an app by its bundle ID (iOS) or package (Android), or
// brings it to the front, granting the permissions named first so it asks
// for none.
func (m *Manager) Launch(ctx context.Context, id, app string, grant []string) error {
	if !appName.MatchString(app) {
		return errInvalid.Errorf("not an app id: %q", app)
	}
	c, unlock, err := m.lockInput(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	if err := c.grant(ctx, app, grant); err != nil {
		return err
	}
	if c.d.Platform != PlatformIOS {
		// Without --pct-syskeys 0, monkey refuses a device with no hardware keys.
		_, err = c.shell(ctx, "monkey -p "+app+" -c android.intent.category.LAUNCHER --pct-syskeys 0 1")
		c.screenChanged()
		return err
	}
	out, err := c.simctl(ctx, "launch", c.d.ID, app)
	c.screenChanged()
	if err != nil {
		return err
	}
	c.waitForApp(ctx, launchedPID(out))
	return nil
}

// launchedPID reads simctl launch's "app.repogo: 4512"; 0 when it says none.
func launchedPID(out []byte) int {
	_, pid, _ := strings.Cut(strings.TrimSpace(string(out)), ": ")
	n, _ := strconv.Atoi(pid)
	return n
}

// appShows bounds waitForApp: an app's first frame comes well inside it.
const appShows = 5 * time.Second

// waitForApp reads the screen until the app with pid is in front, so the
// screen an action answers is the app's, not the one it opened over;
// c.agent.lock held.
func (c *controller) waitForApp(ctx context.Context, pid int) {
	if pid == 0 {
		return
	}
	for start := time.Now(); time.Since(start) < appShows; {
		read, err := c.readElements(ctx)
		if err != nil || read.pid == pid {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(waitPoll):
		}
	}
}

// grant grants an app the permissions named, refusing one this platform
// has no grant for. Android refuses a permission the app doesn't declare,
// which is skipped: the app can't ask for it either.
func (c *controller) grant(ctx context.Context, app string, names []string) error {
	for _, name := range names {
		g, ok := grants[name]
		if !ok {
			return errInvalid.Errorf("unknown permission %q: one of %s", name, strings.Join(GrantNames(), ", "))
		}
		if c.d.Platform == PlatformIOS {
			if g.ios == "" {
				return errInvalid.Errorf("a simulator can't grant %s", name)
			}
			if _, err := c.simctl(ctx, "privacy", c.d.ID, "grant", g.ios, app); err != nil {
				return err
			}
			continue
		}
		if len(g.android) == 0 {
			return errInvalid.Errorf("Android has no %s permission to grant", name)
		}
		for _, permission := range g.android {
			_, err := c.shell(ctx, "pm grant "+app+" android.permission."+permission)
			if err != nil && !strings.Contains(err.Error(), "has not requested permission") &&
				!strings.Contains(err.Error(), "Unknown permission") {
				return err
			}
		}
	}
	return nil
}

// appName is a bundle ID or package: letters, digits, dots, dashes, underscores.
var appName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
