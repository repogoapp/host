package screens

import (
	"testing"

	"github.com/repogoapp/host/internal/screens/capture"
)

func TestWMSizePrefersTheOverride(t *testing.T) {
	if w, h := parseWMSize("Physical size: 1080x2400\n"); w != 1080 || h != 2400 {
		t.Fatalf("physical = %dx%d", w, h)
	}
	if w, h := parseWMSize("Physical size: 1080x2400\nOverride size: 720x1600\n"); w != 720 || h != 1600 {
		t.Fatalf("override = %dx%d", w, h)
	}
}

func TestStreamSizeCapsTheShortSideAt720(t *testing.T) {
	for _, c := range []struct {
		w, h int
		want string
	}{{1080, 2400, "720x1600"}, {2400, 1080, "1600x720"}, {480, 800, "480x800"}, {1080, 2424, "720x1616"}, {0, 0, "0x0"}} {
		if got := streamSize(c.w, c.h); got != c.want {
			t.Errorf("streamSize(%d, %d) = %s, want %s", c.w, c.h, got, c.want)
		}
	}
}

func TestAndroidCommandsFollowTheFinger(t *testing.T) {
	touch := func(phase string, x, y float64) capture.Input { return capture.Input{Touch: phase, X: x, Y: y} }
	for _, c := range []struct {
		in   capture.Input
		want string
	}{
		{touch("down", 0.5, 0.5), "input motionevent DOWN 540 1200"},
		{touch("move", 0.5, 0.25), "input motionevent MOVE 540 600"},
		{touch("up", 0, 1), "input motionevent UP 0 2400"},
		{touch("cancel", 0, 1), "input motionevent CANCEL 0 2400"},
		{capture.Input{Button: "back"}, "input keyevent KEYCODE_BACK"},
		{capture.Input{Button: "recents"}, "input keyevent KEYCODE_APP_SWITCH"},
		{capture.Input{Button: "siri"}, ""},
	} {
		if got := androidCommand(c.in, 1081, 2401); got != c.want {
			t.Errorf("androidCommand(%+v) = %q, want %q", c.in, got, c.want)
		}
	}
}
