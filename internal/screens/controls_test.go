package screens

import (
	"slices"
	"testing"

	"github.com/repogoapp/host/internal/screens/capture"
)

func TestTurnGoesRoundBothWays(t *testing.T) {
	o := 1
	for _, want := range []int{3, 2, 4, 1} {
		if o = turn(o, "left"); o != want {
			t.Fatalf("left = %d, want %d", o, want)
		}
	}
	for _, want := range []int{4, 2, 3, 1} {
		if o = turn(o, "right"); o != want {
			t.Fatalf("right = %d, want %d", o, want)
		}
	}
}

func TestIOSRotationTurnsTheFramebufferUpright(t *testing.T) {
	for o, want := range map[int]int{1: 0, 2: 180, 3: -90, 4: 90} {
		if got := iosRotation(o); got != want {
			t.Errorf("iosRotation(%d) = %d, want %d", o, got, want)
		}
	}
}

func TestUprightTurnsLikeCoreDeviceReports(t *testing.T) {
	for degrees, want := range map[int]int{0: 0, 90: 90, 180: 180, 270: -90, -90: -90} {
		if got := upright(degrees); got != want {
			t.Errorf("upright(%d) = %d, want %d", degrees, got, want)
		}
	}
}

func TestSimcapDisplayAddsFoldAndSetsRotation(t *testing.T) {
	var replies []Message
	c := &controller{d: Screen{Platform: "ios", OS: "iOS 27.1"}, reply: func(v any) error {
		replies = append(replies, v.(Message))
		return nil
	}}
	c.fromSimcap([]byte(`{"display":{"screen":2,"rotation":90,"foldable":true}}`))
	if !slices.Contains(c.controls(), "fold") || c.state.Rotation != 90 {
		t.Fatalf("controls %v rotation %d", c.controls(), c.state.Rotation)
	}
	if len(replies) != 2 || replies[0].Controls == nil || replies[1].State == nil {
		t.Fatalf("replies %+v, want the new controls then the state", replies)
	}
	c.fromSimcap([]byte(`{"failed":"couldn't fold to tent"}`))
	if replies[len(replies)-1].Failed != "couldn't fold to tent" {
		t.Fatalf("failure not passed on: %+v", replies[len(replies)-1])
	}
}

func TestIOSMajorReadsTheRuntime(t *testing.T) {
	if iosMajor("iOS 27.1") != 27 || iosMajor("iOS 18.2") != 18 || iosMajor("iOS") != 0 {
		t.Fatal("iosMajor misread a runtime")
	}
}

func TestNearestSizeRoundsAndroidScales(t *testing.T) {
	for scale, want := range map[float64]string{1.0: "large", 1.3: "extra-extra-extra-large", 0.87: "small", 2.0: "accessibility-extra-extra-extra-large"} {
		if got := nearestSize(scale); got != want {
			t.Errorf("nearestSize(%g) = %s, want %s", scale, got, want)
		}
	}
}

func TestHostRunsActionsButNotTouches(t *testing.T) {
	ios := &controller{d: Screen{Platform: "ios", OS: "iOS 26.3"}}
	old := &controller{d: Screen{Platform: "ios", OS: "iOS 18.2"}}
	android := &controller{d: Screen{Platform: "android"}}
	for _, c := range []struct {
		c    *controller
		in   capture.Input
		want bool
	}{
		{ios, capture.Input{Touch: "down"}, false},
		{ios, capture.Input{Action: "appearance", Value: "dark"}, true},
		{ios, capture.Input{Action: capture.ActionText, Value: "hi"}, false},
		{ios, capture.Input{Button: "home"}, true},
		{old, capture.Input{Button: "home"}, false},
		{android, capture.Input{Button: "home"}, false},
		// Typing and keys keep their order with touches, on the device's path.
		{android, capture.Input{Action: "text", Value: "hi"}, false},
		{android, capture.Input{Key: &capture.Key{Keysym: 0xff08, Down: true}}, false},
		{android, capture.Input{Action: "url", Value: "https://example.com"}, true},
	} {
		if got := c.c.hostRuns(c.in); got != c.want {
			t.Errorf("%s %+v: hostRuns = %v, want %v", c.c.d.OS, c.in, got, c.want)
		}
	}
}

func TestIOS27OffersTypingOnlyWithUniversalHID(t *testing.T) {
	c := &controller{d: Screen{Platform: "ios", OS: "iOS 27.0"}}
	if !c.universalHID() {
		t.Fatal("iOS 27 takes input through Universal HID")
	}
	controls := c.controls()
	if slices.Contains(controls, "text") != capture.UniversalHID {
		t.Fatalf("controls %v: text offered without the bridge, or missing with it", controls)
	}
	for _, button := range []string{"lock", "volume_up", "siri"} {
		if slices.Contains(controls, button) {
			t.Fatalf("controls %v offer %s, which iOS 27 ignores", controls, button)
		}
	}
	// The on-screen keyboard is a CoreSimulator setting, not an HID message.
	if !slices.Contains(controls, "keyboard") {
		t.Fatalf("controls %v lack the on-screen keyboard", controls)
	}
	old := &controller{d: Screen{Platform: "ios", OS: "iOS 26.3"}}
	if old.universalHID() || !slices.Contains(old.controls(), "lock") {
		t.Fatal("iOS 26 keeps its Indigo buttons")
	}
}
