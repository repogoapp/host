package screens

import (
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/screens/capture"
)

// fakeEvents keeps what the screens publish, in order.
type fakeEvents struct{ sent []emit.Stateful }

func (f *fakeEvents) Publish(_ emit.Room, ev emit.Stateful, _ func(emit.Event, bool) emit.Verdict) bool {
	f.sent = append(f.sent, ev)
	return true
}
func (f *fakeEvents) Join(device.ID, emit.Room) emit.Snapshot { return emit.Snapshot{} }
func (f *fakeEvents) Leave(device.ID, emit.Room)              {}

func testController(d Screen) (*controller, *fakeEvents) {
	events := &fakeEvents{}
	return newController(d, NewManager(events)), events
}

func TestTurnGoesRoundBothWays(t *testing.T) {
	o := 1
	for _, want := range []int{3, 2, 4, 1} {
		if o = turn(o, TurnLeft); o != want {
			t.Fatalf("left = %d, want %d", o, want)
		}
	}
	for _, want := range []int{4, 2, 3, 1} {
		if o = turn(o, TurnRight); o != want {
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

// turns is the turn a controller's views were last told to give their frames.
func turns(c *controller) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return ((c.rotation/90)%4 + 4) % 4
}

// A second panel adds the pose control, and the panel's turn turns the
// frames from then on; each change sends the view.
func TestSimcapDisplayAddsPoseAndTurnsTheFrames(t *testing.T) {
	c, events := testController(Screen{ID: "A", Kind: KindSimulator, Platform: PlatformIOS, OS: OS{Name: "iOS", Version: "27.1"}})
	c.fromSimcap([]byte(`{"display":{"screen":2,"rotation":90,"foldable":true}}`))
	if !slices.Contains(c.controls(), ControlPose) || turns(c) != 1 {
		t.Fatalf("controls %v turns %d", c.controls(), turns(c))
	}
	view, ok := events.sent[len(events.sent)-1].(ViewChanged)
	if !ok || !slices.Contains(view.Controls, ControlPose) || view.State == nil {
		t.Fatalf("sent %+v, want the view with pose", events.sent)
	}
	c.fromSimcap([]byte(`{"display":{"screen":2,"rotation":270}}`))
	if turns(c) != 3 {
		t.Fatalf("turns %d, want 3", turns(c))
	}
}

// An action simcap reports done, failed or not, answers the one waiting for it.
func TestSimcapDoneAnswersItsCommand(t *testing.T) {
	c, _ := testController(Screen{ID: "A", Kind: KindSimulator, Platform: PlatformIOS})
	done := make(chan commandDone, 1)
	c.commands[7] = done
	c.fromSimcap([]byte(`{"done":{"id":7,"error":"couldn't fold to tent"}}`))
	if d := <-done; d.err == nil || d.err.Error() != "couldn't fold to tent" {
		t.Fatalf("err = %v", d.err)
	}
	c.commands[8] = done
	c.fromSimcap([]byte(`{"done":{"id":8,"data":{"width":402}}}`))
	if d := <-done; d.err != nil || string(d.data) != `{"width":402}` {
		t.Fatalf("done = %v %s", d.err, d.data)
	}
}

func TestActionsWithoutALiveViewSaySo(t *testing.T) {
	c, _ := testController(Screen{ID: "A", Kind: KindSimulator, Platform: PlatformIOS})
	err := c.forward(capture.Input{Action: capture.ActionMemoryWarning})
	if !errors.Is(err, ErrNotStreaming) || errkind.Of(err) != errkind.Unavailable {
		t.Fatalf("err = %v (kind %v)", err, errkind.Of(err))
	}
	if data, ok := err.(*Error); !ok || data.ErrorData().(ErrorData).Code != ErrorNotStreaming {
		t.Fatalf("err %v carries no not_streaming code", err)
	}
}

func TestIOSMajorReadsTheVersion(t *testing.T) {
	if iosMajor("27.1") != 27 || iosMajor("18.2") != 18 || iosMajor("") != 0 {
		t.Fatal("iosMajor misread a version")
	}
}

func TestNearestSizeRoundsAndroidScales(t *testing.T) {
	for scale, want := range map[float64]ContentSize{1.0: "large", 1.3: "extra-extra-extra-large", 0.87: "small", 2.0: "accessibility-extra-extra-extra-large"} {
		if got := nearestSize(scale); got != want {
			t.Errorf("nearestSize(%g) = %s, want %s", scale, got, want)
		}
	}
}

// What the phone sends goes to the device's path in order, except what the
// host does itself: iOS 26's Home, and typing that needs the pasteboard.
func TestTranslateSplitsTheDevicePathFromTheHosts(t *testing.T) {
	ios26, _ := testController(Screen{ID: "A", Kind: KindSimulator, Platform: PlatformIOS, OS: OS{Name: "iOS", Version: "26.3"}})
	ios18, _ := testController(Screen{ID: "B", Kind: KindSimulator, Platform: PlatformIOS, OS: OS{Name: "iOS", Version: "18.2"}})
	android, _ := testController(Screen{ID: "C", Kind: KindEmulator, Platform: PlatformAndroid})
	home := StreamInput{Type: "button", Button: &ButtonInput{Button: ButtonHome}}
	if items := ios26.translate(home); len(items) != 1 || items[0].host == nil {
		t.Fatalf("iOS 26 home = %+v, want the host's springboard launch", items)
	}
	if items := ios18.translate(home); len(items) != 1 || items[0].device.Button != "home" {
		t.Fatalf("iOS 18 home = %+v", items)
	}
	if items := android.translate(StreamInput{Type: "button", Button: &ButtonInput{Button: ButtonSiri}}); len(items) != 0 {
		t.Fatalf("Android took Siri: %+v", items)
	}
	second := StreamInput{Type: "touch", Touch: &TouchInput{Finger: 1, Phase: TouchDown, X: 0.5, Y: 0.5}}
	if items := android.translate(second); len(items) != 0 {
		t.Fatalf("a second finger was played: %+v", items)
	}
	text := StreamInput{Type: "text", Text: &TextInput{Text: "café"}}
	if items := ios26.translate(text); len(items) != 1 || items[0].host == nil {
		t.Fatalf("iOS text = %+v, want the host's typing", items)
	}
	if items := android.translate(text); len(items) != 1 || items[0].device.Value != "café" {
		t.Fatalf("Android text = %+v", items)
	}
	pointer := StreamInput{Type: "pointer", Pointer: &PointerInput{X: 0.1, Y: 0.2, Left: true, Right: true}}
	if items := android.translate(pointer); len(items) != 1 || items[0].device.Pointer.Buttons != 5 {
		t.Fatalf("pointer = %+v", items)
	}
}

func TestSplitTypeableKeepsOrder(t *testing.T) {
	runs := splitTypeable("ab é!😀c")
	var got []string
	for _, r := range runs {
		got = append(got, r.text)
	}
	if want := []string{"ab ", "é", "!", "😀", "c"}; !slices.Equal(got, want) {
		t.Fatalf("runs %q, want %q", got, want)
	}
}

func TestIOS27OffersTypingOnlyWithUniversalHID(t *testing.T) {
	c, _ := testController(Screen{Kind: KindSimulator, Platform: PlatformIOS, OS: OS{Name: "iOS", Version: "27.0"}})
	if !c.universalHID() {
		t.Fatal("iOS 27 takes input through Universal HID")
	}
	if slices.Contains(c.controls(), ControlText) != capture.UniversalHID {
		t.Fatalf("controls %v: text offered without the bridge, or missing with it", c.controls())
	}
	for _, b := range []HardwareButton{ButtonLock, ButtonVolumeUp, ButtonSiri} {
		if slices.Contains(c.buttons(), b) {
			t.Fatalf("buttons %v offer %s, which iOS 27 ignores", c.buttons(), b)
		}
	}
	// The on-screen keyboard is a CoreSimulator setting, not an HID message.
	if !slices.Contains(c.controls(), ControlOnscreenKeyboard) {
		t.Fatalf("controls %v lack the on-screen keyboard", c.controls())
	}
	old, _ := testController(Screen{Kind: KindSimulator, Platform: PlatformIOS, OS: OS{Name: "iOS", Version: "26.3"}})
	if old.universalHID() || !slices.Contains(old.buttons(), ButtonLock) {
		t.Fatal("iOS 26 keeps its Indigo buttons")
	}
}

// Moves fold into the latest; a press, a lift or a key is never dropped.
func TestInputQueueFoldsMovesAndKeepsTransitions(t *testing.T) {
	q := newInputQueue()
	touch := func(phase string, x float64) item { return item{device: capture.Input{Touch: phase, X: x}} }
	q.push(touch("down", 0.1))
	for i := range 2000 {
		q.push(touch("move", float64(i)/2000))
	}
	q.push(touch("up", 0.9))
	for range 600 {
		q.push(item{device: capture.Input{Key: &capture.Key{Keysym: 'a', Down: true}}})
	}
	q.close()
	var phases []string
	keys := 0
	for {
		it, ok := q.next(t.Context())
		if !ok {
			break
		}
		if it.device.Key != nil {
			keys++
			continue
		}
		phases = append(phases, it.device.Touch)
	}
	if !slices.Equal(phases, []string{"down", "move", "up"}) || keys != 600 {
		t.Fatalf("phases %v keys %d", phases, keys)
	}
}

func TestNegotiatePicksTheNewestCommonVersion(t *testing.T) {
	ready, err := negotiate(StreamRequest{Versions: []int{1, 2}, Codecs: []Codec{CodecHEVC, CodecH264}})
	if err != nil || ready.Version != 1 || ready.Codec != CodecH264 {
		t.Fatalf("ready %+v err %v", ready, err)
	}
	_, err = negotiate(StreamRequest{Versions: []int{2}, Codecs: []Codec{CodecH264}})
	var coded *Error
	if !errors.As(err, &coded) || coded.Code != ErrorUnsupportedVersion {
		t.Fatalf("version 2 only: %v", err)
	}
}

// Every frame carries its size, codec, flags, turn, sequence and time; the
// turn is 0 on a turned simulator too, its frames turned before they're sent.
func TestFrameHeader(t *testing.T) {
	c, _ := testController(Screen{Kind: KindSimulator, Platform: PlatformIOS})
	c.rotation = -90
	var sent [][]byte
	f := &framer{c: c, send: func(b []byte) error { sent = append(sent, b); return nil }}
	nal := func(typ byte) []byte { return append(binary.BigEndian.AppendUint32(nil, 2), typ, 0x80) }
	keyframe := append(append(nal(7), nal(8)...), nal(5)...)
	f.frame(keyframe, 1234)
	f.frame(nal(1), 0)
	h := sent[0]
	if h[0] != 16 || h[1] != 1 || h[2] != frameKeyframe|frameParameterSets || h[3] != 0 ||
		binary.BigEndian.Uint32(h[4:]) != 0 || binary.BigEndian.Uint64(h[8:]) != 1234 || len(h) != 16+len(keyframe) {
		t.Fatalf("keyframe header % x", h[:16])
	}
	if p := sent[1]; p[2] != 0 || binary.BigEndian.Uint32(p[4:]) != 1 {
		t.Fatalf("second header % x", p[:16])
	}
}

func TestOrientationFollowsTheDisplaySimcapReports(t *testing.T) {
	for degrees, want := range map[int]int{0: 1, 90: 4, 180: 2, 270: 3, -90: 3} {
		if got := orientationAt(degrees); got != want {
			t.Fatalf("orientationAt(%d) = %d, want %d", degrees, got, want)
		}
	}
	c, _ := testController(Screen{ID: "duo", Platform: PlatformIOS, Kind: KindSimulator})
	c.fromSimcap([]byte(`{"display":{"screen":3,"rotation":270,"foldable":true}}`))
	c.mu.Lock()
	orientation := c.orientation
	c.mu.Unlock()
	if orientation != 3 {
		t.Fatalf("orientation after a reported 270 = %d, want 3", orientation)
	}
}

// A view is told the turn as it opens; a rotate turns every view's frames as
// it starts, and back, with a frame at the previous turn, when it fails.
func TestARotateTurnsTheViewsFrames(t *testing.T) {
	c, _ := testController(Screen{ID: "duo", Platform: PlatformIOS, Kind: KindSimulator})
	c.rotation = 90
	var got []string
	close := c.open(func(in capture.Input) error {
		got = append(got, in.Action+":"+in.Value)
		return nil
	})
	defer close()
	was := c.aimAt(3)
	c.missed(was)
	want := []string{"turn:1", "turn:3", "turn:1", "keyframe:"}
	if !slices.Equal(got, want) {
		t.Fatalf("view got %q, want %q", got, want)
	}
}

func TestParseOrientationReadsDevicectl(t *testing.T) {
	for out, want := range map[string]int{
		"Current Device Orientation: portrait\n":                  1,
		"Current Device Orientation: portraitUpsideDown\n":        2,
		"12:00 note\nCurrent Device Orientation: landscapeLeft\n": 3,
		"New Device Orientation: landscapeRight":                  4,
	} {
		if got, ok := parseOrientation(out); !ok || got != want {
			t.Fatalf("parseOrientation(%q) = %d, %v; want %d", out, got, ok, want)
		}
	}
	for _, out := range []string{"", "Current Device Orientation: faceUp\n", "ERROR: The specified device was not found."} {
		if _, ok := parseOrientation(out); ok {
			t.Fatalf("parseOrientation(%q) read an orientation", out)
		}
	}
}
