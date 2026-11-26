package screens

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/repogoapp/host/internal/screens/capture"
)

// Reading a screen for agents, and aiming at what it shows: each read is
// kept as the last one, numbered for refs, and forgotten when the screen
// changes; a read after a fold or turn waits for it to show.

// screenChanged forgets the last read: the screen may no longer be it.
func (c *controller) screenChanged() {
	c.mu.Lock()
	c.screenChangedLocked()
	c.mu.Unlock()
}

// screenChangedLocked is screenChanged with c.mu held.
func (c *controller) screenChangedLocked() {
	c.agent.generation++
	if c.agent.read != nil {
		c.agent.stale = c.agent.read
	}
	c.agent.read = nil
}

// lastRead is the last read, while the screen hasn't changed since; nil otherwise.
func (c *controller) lastRead() *Elements {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.agent.read
}

// readElements reads the screen and keeps it as the last read; c.agent.lock held.
func (c *controller) readElements(ctx context.Context) (Elements, error) {
	var e Elements
	var err error
	c.mu.Lock()
	generation, closed := c.agent.generation, c.agent.closed
	c.mu.Unlock()
	// A shut-down screen starts nothing new, not even a reader.
	if closed {
		return Elements{}, ErrNotRunning
	}
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
	coverKeyboard(&e)
	c.refreshPose(ctx)
	c.mu.Lock()
	c.agent.reads++
	if c.foldable || c.d.Form == FormFoldable {
		e.Pose = c.state.Pose
	}
	// What's still there keeps its ref, so an action's answer can list only
	// what changed, and a ref the agent holds outlives a read.
	before := c.agent.read
	if before == nil {
		before = c.agent.stale
	}
	carryRefs(before, &e, c.agent.reads)
	// A read the screen changed under is kept only as the stale read: its
	// refs are found again by what they named, never aimed at as they are.
	if c.agent.generation == generation {
		c.agent.read = &e
	} else {
		c.agent.stale = &e
	}
	c.mu.Unlock()
	return e, nil
}

// keyboardAnchors are ids only the iOS keyboard's keys have: its keys come
// as buttons among the app's own, marked by nothing else.
var keyboardAnchors = map[string]bool{"space": true, "dictation": true}

// splitKeyboard takes the on-screen keyboard's keys out of an iOS read, as
// Android's reader leaves out its window: the run of buttons around an
// anchor key, none above the rows a keyboard has. It marks the read as
// having a keyboard, and where it starts.
func splitKeyboard(e *Elements) {
	anchor := -1
	for i, el := range e.Elements {
		if el.Role == "Button" && keyboardAnchors[el.ID] {
			anchor = i
			break
		}
	}
	if anchor < 0 {
		return
	}
	// A keyboard is at most five rows of keys: nothing higher is one of them.
	key := e.Elements[anchor]
	lowest := key.Y - 4*key.H
	isKey := func(el Element) bool { return el.Role == "Button" && el.Y >= lowest }
	first, last := anchor, anchor
	for first > 0 && isKey(e.Elements[first-1]) {
		first--
	}
	for last < len(e.Elements)-1 && isKey(e.Elements[last+1]) {
		last++
	}
	top := key.Y
	for _, el := range e.Elements[first : last+1] {
		top = min(top, el.Y)
	}
	e.Elements = append(e.Elements[:first], e.Elements[last+1:]...)
	e.Keyboard, e.keyboardTop = true, top
}

// carryRefs names each element of a read: the ref it had in the read before
// when one there had its role, label and id (the nearest, where several
// did), else a new one of this read, e<read>.<n>.
func carryRefs(before *Elements, e *Elements, read int) {
	used := map[string]bool{}
	for i := range e.Elements {
		el := &e.Elements[i]
		if was, ok := nearestSame(before, *el, used); ok {
			el.Ref = was.Ref
			used[was.Ref] = true
			continue
		}
		el.Ref = fmt.Sprintf("e%d.%d", read, i+1)
	}
}

// nearestSame is the element of a read with el's role, label and id, the one
// nearest it where several have them, skipping refs already taken.
func nearestSame(read *Elements, el Element, used map[string]bool) (Element, bool) {
	if read == nil {
		return Element{}, false
	}
	best, found, bestDistance := Element{}, false, math.Inf(1)
	for _, was := range read.Elements {
		if used[was.Ref] || was.Role != el.Role || was.Label != el.Label || was.ID != el.ID {
			continue
		}
		if d := math.Hypot(was.X+was.W/2-el.X-el.W/2, was.Y+was.H/2-el.Y-el.H/2); d < bestDistance {
			best, found, bestDistance = was, true, d
		}
	}
	return best, found
}

// coverKeyboard marks what lies under the on-screen keyboard, the field
// being typed in aside: a tap there would hit a key.
func coverKeyboard(e *Elements) {
	if !e.Keyboard || e.keyboardTop <= 0 {
		return
	}
	for i := range e.Elements {
		el := &e.Elements[i]
		el.Covered = !el.Focused && el.Y+el.H/2 >= e.keyboardTop
	}
}

// moving notes a fold or turn about to be asked for, so the next read waits
// for the screen to show it.
func (c *controller) moving() {
	c.mu.Lock()
	c.agent.movedAt = time.Now()
	// The last read's size and refs are about to be wrong.
	c.screenChangedLocked()
	c.mu.Unlock()
}

// Settling after a fold or turn: the simulator takes the command at once,
// then reports its new panel 1 to 2 seconds later, and the app lays out
// after that. A read in between sees the old screen, or half of each.
const (
	screenQuiet    = 600 * time.Millisecond // after the panel report
	panelReportMax = 4 * time.Second        // a pose that keeps its panel reports none; a view's first fold is slow
	settleMax      = 6 * time.Second
)

// waitForScreen waits until a fold or turn asked for has shown: its panel
// reported, then a quiet moment. A panel change nobody asked for (the
// phone's own fold) gets the quiet moment too.
func (c *controller) waitForScreen(ctx context.Context) {
	for {
		c.mu.Lock()
		moved, changed := c.agent.movedAt, c.displayChangedAt
		c.mu.Unlock()
		now := time.Now()
		settled := now.Sub(changed) >= screenQuiet
		if !moved.IsZero() && now.Sub(moved) < settleMax {
			reported := changed.After(moved)
			settled = (reported && settled) || (!reported && now.Sub(moved) >= panelReportMax)
		}
		if settled {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (c *controller) readIOSElements(ctx context.Context) (Elements, error) {
	c.waitForScreen(ctx)
	path, err := c.inputPath(ctx)
	if err != nil {
		return Elements{}, err
	}
	var answer struct {
		Elements
		PID   int    `json:"pid"`
		Error string `json:"error"`
	}
	// An app between two layouts (a fold, a launch) can answer an empty
	// tree for a moment: a read that finds nothing tries again before saying so.
	for attempt := 0; ; attempt++ {
		data, err := c.commandVia(ctx, path, capture.Input{Action: capture.ActionElements})
		if err != nil {
			return Elements{}, err
		}
		answer.Elements, answer.Error = Elements{}, ""
		if err := json.Unmarshal(data, &answer); err != nil {
			return Elements{}, fmt.Errorf("the simulator's screen read wrong: %w", err)
		}
		if answer.Error != "" {
			return Elements{}, errors.New(answer.Error)
		}
		if len(answer.Elements.Elements) > 0 || attempt >= 3 {
			break
		}
		select {
		case <-ctx.Done():
			return Elements{}, ctx.Err()
		case <-time.After(400 * time.Millisecond):
		}
	}
	answer.Unit = "pt"
	answer.pid = answer.PID
	splitKeyboard(&answer.Elements)
	for i := range answer.Elements.Elements {
		// A tab or radio button reports being picked as its value.
		el := &answer.Elements.Elements[i]
		switch {
		case el.Role == "RadioButton" || el.Role == "Tab":
			el.Selected = el.Selected || el.Value == "1"
			el.Value = ""
		// A switch or checkbox reports on or off as its value.
		case (el.Role == "CheckBox" || el.Role == "Switch") && (el.Value == "0" || el.Value == "1"):
			el.Checked = ptr(el.Value == "1")
			el.Value = ""
		}
	}
	return answer.Elements, nil
}

// aim is where a target is, reading the screen when it names a label;
// c.agent.lock held.
func (c *controller) aim(ctx context.Context, t Target) (Point, error) {
	if !exactlyOne(t.Ref != "", t.Label != "", t.X != nil || t.Y != nil) || (t.X == nil) != (t.Y == nil) {
		return Point{}, errInvalid.Errorf("tap takes one of ref, label, or x and y")
	}
	if t.X != nil {
		return Point{X: *t.X, Y: *t.Y}, nil
	}
	var el Element
	if t.Ref != "" {
		found := false
		if el, found = c.lastRead().find(t.Ref); !found {
			var err error
			if el, err = c.resolveStaleRef(ctx, t.Ref); err != nil {
				return Point{}, err
			}
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
	return c.landing(el)
}

// landing is where a tap on el lands: the middle of the part of it on the
// screen. One off the screen, or under the keyboard, is refused: the tap
// would land on something else and still answer as if it worked.
func (c *controller) landing(el Element) (Point, error) {
	read := c.lastRead()
	if read == nil || read.Width < 1 || read.Height < 1 {
		return Point{X: el.X + el.W/2, Y: el.Y + el.H/2}, nil
	}
	if el.Covered {
		return Point{}, errInvalid.Errorf("%s (%s %q) is under the keyboard: hide it first (screens_key hide_keyboard)",
			el.Ref, el.Role, el.Label)
	}
	left, top := max(el.X, 0), max(el.Y, 0)
	right, bottom := min(el.X+el.W, read.Width), min(el.Y+el.H, read.Height)
	if right-left < 1 || bottom-top < 1 {
		return Point{}, errInvalid.Errorf("%s (%s %q) is off the %g×%g screen at %g,%g: scroll it into view first",
			el.Ref, el.Role, el.Label, read.Width, read.Height, el.X, el.Y)
	}
	// An iOS switch's frame is its whole row, label and all, and a tap on the
	// label does nothing: aim at the switch, at the row's trailing end.
	if el.Role == "CheckBox" && el.Checked != nil && el.W >= 100 && right == el.X+el.W {
		return Point{X: right - switchInset, Y: (top + bottom) / 2}, nil
	}
	return Point{X: (left + right) / 2, Y: (top + bottom) / 2}, nil
}

// switchInset is how far an iOS switch's middle sits in from its row's end.
const switchInset = 26

// resolveStaleRef finds again what a ref from the read before the screen
// changed named, in a new read: by its ref, which a read carries over, else
// the nearest element with its role, label and id. The screen often settles
// a moment after a read (a fold's second report), and the same button is
// still there; where it's gone, it says so.
func (c *controller) resolveStaleRef(ctx context.Context, ref string) (Element, error) {
	c.mu.Lock()
	stale := c.agent.stale
	c.mu.Unlock()
	was, ok := stale.find(ref)
	if !ok {
		return Element{}, errInvalid.Errorf("%s isn't on the last read of the screen: read the screen again", ref)
	}
	read, err := c.readElements(ctx)
	if err != nil {
		return Element{}, err
	}
	if same, ok := read.find(ref); ok {
		return same, nil
	}
	same, ok := nearestSame(&read, was, nil)
	if !ok {
		return Element{}, errInvalid.Errorf("%s (%s %q) is gone since it was read: read the screen again", ref, was.Role, was.Label)
	}
	return same, nil
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
		func(e Element) bool { return e.Label == "" && e.isField() && contains(e.Value) },
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

// isField is whether the element takes text: a text field on iOS, an
// EditText on Android.
func (e Element) isField() bool {
	return strings.Contains(e.Role, "Field") || strings.Contains(e.Role, "EditText")
}

// isSystemUI is whether Android's own system UI (the status bar, the
// navigation bar) drew the element.
func (e Element) isSystemUI() bool {
	return strings.HasPrefix(e.ID, "com.android.systemui:")
}
