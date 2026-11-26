package screens

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPoseOfDisplaysReadsTheLitPanel(t *testing.T) {
	displays := func(coverLit, innerLit bool) []byte {
		return []byte(`{"result":{"displays":[` +
			`{"active":` + boolText(coverLit) + `,"primary":true,"type":{"integrated":{}}},` +
			`{"active":` + boolText(innerLit) + `,"primary":false,"type":{"integrated":{}}}]}}`)
	}
	if pose, ok := poseOfDisplays(displays(true, false)); !ok || pose != PoseClosed {
		t.Fatalf("cover lit: %v %v", pose, ok)
	}
	if pose, ok := poseOfDisplays(displays(false, true)); !ok || pose != PoseOpen {
		t.Fatalf("inner lit: %v %v", pose, ok)
	}
	// Neither or both lit says nothing; nor does a phone with one panel.
	for _, out := range [][]byte{displays(false, false), displays(true, true),
		[]byte(`{"result":{"displays":[{"active":true,"primary":true,"type":{"integrated":{}}}]}}`)} {
		if _, ok := poseOfDisplays(out); ok {
			t.Fatalf("no pose expected from %s", out)
		}
	}
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestMergePoseKeepsAHalfOpenPoseWhileTheInnerPanelIsLit(t *testing.T) {
	open, closed, book := PoseOpen, PoseClosed, PoseBook
	for _, c := range []struct {
		read, set *Pose
		want      Pose
	}{
		{&open, &book, PoseBook},     // the panels can't tell book from open
		{&open, &closed, PoseOpen},   // opened outside the host
		{&closed, &book, PoseClosed}, // closed outside the host
		{nil, &book, PoseBook},       // nothing read: what was set
	} {
		if got := mergePose(c.read, c.set); got == nil || *got != c.want {
			t.Fatalf("mergePose(%v, %v) = %v, want %s", c.read, c.set, got, c.want)
		}
	}
}

func TestCarryRefsKeepsWhatStayedAndNumbersWhatsNew(t *testing.T) {
	before := &Elements{Elements: []Element{
		{Ref: "e1.1", Role: "Button", Label: "Send", X: 300, Y: 600, W: 40, H: 40},
		{Ref: "e1.2", Role: "Cell", Label: "Row", Y: 100, H: 40},
		{Ref: "e1.3", Role: "Cell", Label: "Row", Y: 140, H: 40},
	}}
	after := &Elements{Elements: []Element{
		{Role: "Cell", Label: "Row", Y: 150, H: 40}, // moved: the nearer of the two
		{Role: "Button", Label: "Send", X: 300, Y: 560, W: 40, H: 40},
		{Role: "StaticText", Label: "Sent"},
		{Role: "Cell", Label: "Row", Y: 90, H: 40},
	}}
	carryRefs(before, after, 2)
	refs := []string{}
	for _, e := range after.Elements {
		refs = append(refs, e.Ref)
	}
	if want := []string{"e1.3", "e1.1", "e2.3", "e1.2"}; !slices.Equal(refs, want) {
		t.Fatalf("refs %v, want %v", refs, want)
	}
}

func TestChangesSinceListsOnlyWhatChanged(t *testing.T) {
	el := func(ref, label string, y float64) Element {
		return Element{Ref: ref, Role: "Button", Label: label, Y: y, W: 10, H: 10}
	}
	before := &Elements{Width: 100, Height: 200, Elements: []Element{
		el("e1.1", "A", 0), el("e1.2", "B", 20), el("e1.3", "C", 40), el("e1.4", "D", 60)}}
	after := Elements{Width: 100, Height: 200, Elements: []Element{
		el("e1.1", "A", 0), el("e1.2", "B", 20), el("e1.3", "C", 45), el("e2.4", "E", 80)}}
	diff := changesSince(before, after)
	if !diff.Changes || diff.Unchanged != 2 || !slices.Equal(diff.Gone, []string{"e1.4"}) || len(diff.Elements) != 2 ||
		diff.Elements[0].Ref != "e1.3" || diff.Elements[1].Ref != "e2.4" {
		t.Fatalf("diff %+v", diff)
	}
	// A new screen, or a new size, answers whole.
	other := Elements{Width: 100, Height: 200, Elements: []Element{el("e3.1", "X", 0), el("e3.2", "Y", 10), el("e1.1", "A", 0)}}
	if d := changesSince(before, other); d.Changes || len(d.Elements) != 3 {
		t.Fatalf("mostly new: %+v", d)
	}
	turned := after
	turned.Width, turned.Height = 200, 100
	if d := changesSince(before, turned); d.Changes {
		t.Fatalf("turned: %+v", d)
	}
	if d := changesSince(nil, after); d.Changes {
		t.Fatalf("nothing before: %+v", d)
	}
}

func TestSplitKeyboardTakesOutTheKeysAndCoversWhatsUnder(t *testing.T) {
	read := Elements{Width: 466, Height: 678, Elements: []Element{
		{Role: "Button", Label: "General", Y: 225, H: 52},
		{Role: "Button", Label: "Passcode", Y: 600, H: 52},
		{Role: "TextField", Value: "Search", Y: 343, H: 28, Focused: true},
		{Role: "Button", Label: "close", Y: 338, H: 38},
		{Role: "Button", Label: "Q", Y: 438, H: 53},
		{Role: "Button", Label: "shift", Y: 544, H: 53},
		{Role: "Button", Label: "space", ID: "space", Y: 596, H: 53},
		{Role: "Button", Label: "Dictate", ID: "dictation", Y: 588, H: 69},
	}}
	splitKeyboard(&read)
	coverKeyboard(&read)
	if !read.Keyboard || read.keyboardTop != 438 {
		t.Fatalf("keyboard %v at %v", read.Keyboard, read.keyboardTop)
	}
	labels := []string{}
	for _, e := range read.Elements {
		labels = append(labels, e.Label)
	}
	if !slices.Equal(labels, []string{"General", "Passcode", "", "close"}) {
		t.Fatalf("left %v", labels)
	}
	if read.Elements[0].Covered || !read.Elements[1].Covered || read.Elements[2].Covered {
		t.Fatalf("covered: %+v", read.Elements)
	}
	// No anchor key, no keyboard.
	none := Elements{Elements: []Element{{Role: "Button", Label: "Q", Y: 438}}}
	if splitKeyboard(&none); none.Keyboard || len(none.Elements) != 1 {
		t.Fatalf("no keyboard: %+v", none)
	}
}

func TestLandingAimsAtWhatsOnTheScreen(t *testing.T) {
	c := &controller{}
	c.agent.read = &Elements{Width: 400, Height: 800}
	at, err := c.landing(Element{X: 300, Y: 100, W: 200, H: 40}) // half off the right edge
	if err != nil || at != (Point{X: 350, Y: 120}) {
		t.Fatalf("half on: %v %v", at, err)
	}
	on := true
	if at, _ := c.landing(Element{Role: "CheckBox", Checked: &on, X: 40, Y: 500, W: 322, H: 28}); at != (Point{X: 336, Y: 514}) {
		t.Fatalf("switch row: %v", at)
	}
	if _, err := c.landing(Element{Ref: "e1.1", X: -200, Y: 100, W: 160, H: 40}); err == nil ||
		!strings.Contains(err.Error(), "off the") {
		t.Fatalf("off the screen: %v", err)
	}
	if _, err := c.landing(Element{Ref: "e1.2", Y: 700, W: 40, H: 40, Covered: true}); err == nil ||
		!strings.Contains(err.Error(), "under the keyboard") {
		t.Fatalf("covered: %v", err)
	}
}

func TestDragStartAvoidsControls(t *testing.T) {
	read := Elements{Width: 400, Height: 800, keyboardTop: 500, Elements: []Element{
		{Role: "Button", X: 0, Y: 100, W: 400, H: 200},     // the top half of the space above the keyboard
		{Role: "StaticText", X: 0, Y: 320, W: 400, H: 100}, // text is fine to start on
		{Role: "Group", X: 0, Y: 0, W: 400, H: 800},        // a container is too
		{Role: "Button", X: 150, Y: 340, W: 100, H: 40},    // but not a button in it
	}}
	p, ok := dragStart(read)
	if !ok || onControl(read, p) || p.Y >= 492 || p.Y <= 308 {
		t.Fatalf("start %v %v", p, ok)
	}
	full := Elements{Width: 400, Height: 800, keyboardTop: 500, Elements: []Element{
		{Role: "Button", W: 400, H: 300}, {Role: "Button", Y: 300, W: 400, H: 200}}}
	if _, ok := dragStart(full); ok {
		t.Fatal("a screen of controls has no start")
	}
}

func TestLaunchedPID(t *testing.T) {
	if pid := launchedPID([]byte("com.apple.Preferences: 20566\n")); pid != 20566 {
		t.Fatalf("pid %d", pid)
	}
	if pid := launchedPID([]byte("")); pid != 0 {
		t.Fatalf("pid %d", pid)
	}
}

func TestLineWritesStates(t *testing.T) {
	on := true
	line := Element{Ref: "e1.1", Role: "CheckBox", Label: "Dark", Checked: &on, Disabled: true, Covered: true}.Line()
	if !strings.Contains(line, `"Dark" on disabled covered`) {
		t.Fatalf("line %q", line)
	}
}

func TestHeldRefusesAgentsRightAfterThePhonesInput(t *testing.T) {
	c := &controller{}
	if err := c.held(); err != nil {
		t.Fatalf("no phone input: %v", err)
	}
	c.phoneUsed()
	if err := c.held(); err == nil || !strings.Contains(err.Error(), "from the RepoGo app") {
		t.Fatalf("right after the phone: %v", err)
	}
	c.agent.phoneAt = time.Now().Add(-phoneHold)
	if err := c.held(); err != nil {
		t.Fatalf("after the hold: %v", err)
	}
}
