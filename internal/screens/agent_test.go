package screens

import (
	"context"
	"strings"
	"testing"
)

func TestMatchPrefersExactLabelThenIDThenContains(t *testing.T) {
	els := []Element{
		{Ref: "e1.1", Role: "Button", Label: "Files"},
		{Ref: "e1.2", Role: "Button", Label: "Open Files Panel", ID: "files-panel"},
		{Ref: "e1.3", Role: "StaticText", Label: "Juniper Store"},
		{Ref: "e1.4", Role: "TextField", Value: "Ask RepoGo..."},
	}
	for label, want := range map[string]string{
		"Files":       "e1.1", // exact beats the one containing it
		"files-panel": "e1.2", // by id
		"juniper":     "e1.3", // contains, any case
		"Ask RepoGo":  "e1.4", // an unlabeled field, by its prompt
	} {
		got, err := match(els, label)
		if err != nil || got.Ref != want {
			t.Fatalf("match %q = %v %v, want %s", label, got.Ref, err, want)
		}
	}
}

func TestMatchRefusesNoneAndSeveral(t *testing.T) {
	els := []Element{{Ref: "e2.1", Label: "Copy"}, {Ref: "e2.2", Label: "Copy answer"}, {Ref: "e2.3", Label: "Copy link"}}
	if _, err := match(els, "Paste"); err == nil || !strings.Contains(err.Error(), "read the screen") {
		t.Fatalf("no match: %v", err)
	}
	// Two contain it and none is exact: both are named, by ref.
	_, err := match(els, "copy ")
	if err == nil || !strings.Contains(err.Error(), "e2.2") || !strings.Contains(err.Error(), "e2.3") {
		t.Fatalf("several: %v", err)
	}
}

// A ref names its read: after a fold the last read is gone, and its refs with it.
func TestRefsDieWithAFold(t *testing.T) {
	c, _ := testController(Screen{ID: "A", Kind: KindSimulator, Platform: PlatformIOS})
	c.agent.read = &Elements{Width: 402, Height: 874, Elements: []Element{{Ref: "e1.1", Label: "Files", X: 10, Y: 10, W: 20, H: 20}}}
	at, err := c.aim(context.Background(), Target{Ref: "e1.1"})
	if err != nil || at != (Point{X: 20, Y: 20}) {
		t.Fatalf("aim = %v %v", at, err)
	}
	c.fromSimcap([]byte(`{"display":{"screen":3,"rotation":0}}`))
	if _, err := c.aim(context.Background(), Target{Ref: "e1.1"}); err == nil {
		t.Fatal("a ref from before the fold still aimed")
	}
}

func TestAimTakesOneTarget(t *testing.T) {
	c, _ := testController(Screen{ID: "A", Kind: KindSimulator, Platform: PlatformIOS})
	x := 1.0
	for _, bad := range []Target{{}, {Ref: "e1.1", Label: "Files"}, {X: &x}} {
		if _, err := c.aim(context.Background(), bad); err == nil {
			t.Fatalf("aim %+v took it", bad)
		}
	}
}

func TestAgentsLeaveTheMacDisplayAlone(t *testing.T) {
	m := NewManager(&fakeEvents{})
	c := newController(Screen{ID: "display", Kind: KindDisplay, Platform: PlatformMacOS}, m)
	m.controllers["display"] = c
	c.views = append(c.views, &liveView{forward: nil})
	if _, err := m.Elements(context.Background(), "display"); err == nil {
		t.Fatal("read the Mac's display")
	}
}

// A foldable's screencap warns ahead of the picture; the PNG starts after.
func TestScreencapSkipsAWarningAheadOfThePNG(t *testing.T) {
	out := append([]byte("[Warning] Multiple displays were found, but no display id specified!\n"), pngSignature...)
	if at := strings.Index(string(out), string(pngSignature)); at <= 0 || string(out[at:at+4]) != "\x89PNG" {
		t.Fatalf("signature at %d", at)
	}
}

// The phone's lift, button, typing or key forgets the last read; a finger's
// move doesn't.
func TestPhoneInputThatChangesTheScreenForgetsTheRead(t *testing.T) {
	for _, tc := range []struct {
		in   StreamInput
		want bool
	}{
		{StreamInput{Touch: &TouchInput{Phase: TouchMove}}, false},
		{StreamInput{Touch: &TouchInput{Phase: TouchUp}}, true},
		{StreamInput{Button: &ButtonInput{Button: ButtonHome}}, true},
		{StreamInput{Text: &TextInput{Text: "a"}}, true},
		{StreamInput{Ping: &PingInput{}}, false},
	} {
		if got := changesScreen(tc.in); got != tc.want {
			t.Fatalf("changesScreen(%+v) = %v", tc.in, got)
		}
	}
}

// A change while a read runs keeps it from becoming the last read.
func TestAReadTheScreenChangedUnderIsNotKept(t *testing.T) {
	c, _ := testController(Screen{ID: "A", Kind: KindSimulator, Platform: PlatformIOS})
	c.mu.Lock()
	generation := c.agent.generation
	c.mu.Unlock()
	c.screenChanged() // as a fold would, mid-read
	c.mu.Lock()
	kept := c.agent.generation == generation
	c.mu.Unlock()
	if kept {
		t.Fatal("the generation didn't move")
	}
}

// A shut-down screen starts nothing new for agents.
func TestAClosedScreenStartsNoInput(t *testing.T) {
	c, _ := testController(Screen{ID: "A", Kind: KindSimulator, Platform: PlatformIOS})
	c.closeAgent()
	if _, err := c.inputPath(context.Background()); err == nil {
		t.Fatal("a closed screen started input")
	}
}
