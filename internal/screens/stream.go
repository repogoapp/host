package screens

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/screens/capture"
)

// The /stream protocol: the phone opens with a StreamRequest; the host
// answers StreamMessage{ready}, then frames (a 16-byte header and an access
// unit) until StreamMessage{failed} or the socket closes. The phone sends
// StreamInput. Either side skips a type, codec or flag it doesn't know; the
// version bumps only for a change the other side can't skip.

// StreamVersion is the newest /stream version this host speaks.
const StreamVersion = 1

// Codec is how a stream's frames are encoded.
type Codec string

const (
	CodecH264 Codec = "h264"
	CodecHEVC Codec = "hevc"
)

func (Codec) Values() []string { return []string{"h264", "hevc"} }

// StreamRequest is a /stream socket's first message: the versions and
// codecs the phone takes, whose screen to send, and for a display the
// account password the phone keeps; never logged.
type StreamRequest struct {
	Versions []int   `json:"versions" wire:"array"`
	Codecs   []Codec `json:"codecs" wire:"array"`
	Screen   string  `json:"screen"`
	Password string  `json:"password,omitempty"`
}

// StreamMessage is a text message the host sends on /stream.
type StreamMessage struct {
	Type   string        `json:"type"`
	Ready  *StreamReady  `json:"ready,omitempty"`
	Failed *StreamFailed `json:"failed,omitempty"`
	Audio  *StreamAudio  `json:"audio,omitempty"`
	Mic    *StreamMic    `json:"mic,omitempty"`
	Edge   *StreamEdge   `json:"edge,omitempty"`
	Pong   *StreamPong   `json:"pong,omitempty"`
	Stats  *StreamStats  `json:"stats,omitempty"`
}

func (StreamMessage) WireUnion() {}

// StreamPong answers a ping at once, with the host's time then: the phone
// reads the round trip from it, and from the time when frames were
// captured on the host's clock.
type StreamPong struct {
	ID int `json:"id"`
	// HostUS is the host's uptime clock in microseconds, the clock frame
	// headers' capture times use; 0 when it has none.
	HostUS int64 `json:"host_us"`
}

// StreamStats is what the host sent, every half second once frames flow.
type StreamStats struct {
	// SentBytes counts every binary frame sent, video and sound, as the
	// phone's acks do.
	SentBytes int64 `json:"sent_bytes"`
	// InFlightBytes is sent but not yet acked.
	InFlightBytes int64 `json:"in_flight_bytes"`
	// BudgetBytes is how much the pacer lets be in flight, and BitrateBps
	// the encoder's target, while it paces.
	BudgetBytes *int64 `json:"budget_bytes,omitempty"`
	BitrateBps  *int   `json:"bitrate_bps,omitempty"`
	// Frames is the video frames sent in the window, WindowMS long.
	Frames   int `json:"frames"`
	WindowMS int `json:"window_ms"`
	// EncodeMS is the average from capture to send in the window, when the
	// source stamps frames on the host's clock.
	EncodeMS *float64 `json:"encode_ms,omitempty"`
}

// StreamReady is the version and codec the host picked; frames follow.
type StreamReady struct {
	Version int   `json:"version"`
	Codec   Codec `json:"codec"`
}

// AudioCodec is how a stream's sound is encoded.
type AudioCodec string

const AudioCodecAAC AudioCodec = "aac"

func (AudioCodec) Values() []string { return []string{"aac"} }

// StreamAudio is the sound's state, sent when it starts, stops or can't
// run; the video goes on either way. While on, frames with the AAC codec
// in their header carry it: AAC-LC, one raw access unit each, at
// SampleRate with Channels.
type StreamAudio struct {
	On         bool        `json:"on"`
	Codec      *AudioCodec `json:"codec,omitempty"`
	SampleRate *int        `json:"sample_rate,omitempty"`
	Channels   *int        `json:"channels,omitempty"`
	// Error is why the sound stopped or couldn't start.
	Error string `json:"error,omitempty"`
}

// StreamMic is the phone's microphone on the Mac, sent when it starts,
// stops or can't run. While on, the Mac has an input named Device that apps
// can pick, playing the MicAudio the phone sends.
type StreamMic struct {
	On     bool   `json:"on"`
	Device string `json:"device,omitempty"`
	// Error is why the mic stopped or couldn't start.
	Error string `json:"error,omitempty"`
}

// StreamEdge is where the scroll under a Mac's cursor rests, sent after
// the phone's scrolls when it changes: reaching the top or the bottom, or
// leaving one. Any other input (a move, a click, a key) forgets it, on both
// sides, so the next scroll says it again.
type StreamEdge struct {
	Vertical ScrollEdge `json:"vertical"`
}

// ScrollEdge is a scroll's end, or none.
type ScrollEdge string

const (
	ScrollEdgeNone   ScrollEdge = "none"
	ScrollEdgeTop    ScrollEdge = "top"
	ScrollEdgeBottom ScrollEdge = "bottom"
)

func (ScrollEdge) Values() []string { return []string{"none", "top", "bottom"} }

// StreamFailed is why the stream ended, with a code when the phone can act on it.
type StreamFailed struct {
	Error string       `json:"error"`
	Code  *ScreenError `json:"code,omitempty"`
}

// StreamInput is one message the phone sends: a finger, a button, typing,
// a key, a Mac's pointer or wheel, a space swipe, or a keyframe request.
type StreamInput struct {
	Type       string           `json:"type"`
	Touch      *TouchInput      `json:"touch,omitempty"`
	Button     *ButtonInput     `json:"button,omitempty"`
	Text       *TextInput       `json:"text,omitempty"`
	Key        *KeyInput        `json:"key,omitempty"`
	Pointer    *PointerInput    `json:"pointer,omitempty"`
	Scroll     *ScrollInput     `json:"scroll,omitempty"`
	SpaceSwipe *SpaceSwipeInput `json:"space_swipe,omitempty"`
	Keyframe   *KeyframeInput   `json:"keyframe,omitempty"`
	Sound      *SoundInput      `json:"sound,omitempty"`
	Mic        *MicInput        `json:"mic,omitempty"`
	MicAudio   *MicAudioInput   `json:"mic_audio,omitempty"`
	Ack        *AckInput        `json:"ack,omitempty"`
	Ping       *PingInput       `json:"ping,omitempty"`
}

func (StreamInput) WireUnion() {}

// AckInput is how many bytes of binary frames, video and sound, the phone
// has received in all, header included; sent often while frames arrive.
type AckInput struct {
	Bytes int64 `json:"bytes"`
}

// PingInput asks for a pong with the same ID, to time the round trip.
type PingInput struct {
	ID int `json:"id"`
}

// TouchPhase is a finger's step.
type TouchPhase string

const (
	TouchDown   TouchPhase = "down"
	TouchMove   TouchPhase = "move"
	TouchUp     TouchPhase = "up"
	TouchCancel TouchPhase = "cancel" // the phone took the finger for a pinch
)

func (TouchPhase) Values() []string { return []string{"down", "move", "up", "cancel"} }

// TouchInput is a finger on the screen. X and Y are fractions of the frame
// as sent, before the phone turns it by the header's rotation (always 0
// from this host: it sends frames upright). Only finger 0 is played: the
// devices take one finger from the host.
type TouchInput struct {
	Finger int        `json:"finger"`
	Phase  TouchPhase `json:"phase"`
	X      float64    `json:"x"`
	Y      float64    `json:"y"`
}

// ButtonInput presses a hardware button.
type ButtonInput struct {
	Button HardwareButton `json:"button"`
}

// TextInput types text, any Unicode, in order with the rest.
type TextInput struct {
	Text string `json:"text"`
}

// KeyInput presses or releases one key, by its X11 keysym.
type KeyInput struct {
	Keysym int  `json:"keysym"`
	Down   bool `json:"down"`
}

// PointerInput is a Mac's cursor at a fraction of the frame, with the buttons held.
type PointerInput struct {
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Left   bool    `json:"left,omitempty"`
	Right  bool    `json:"right,omitempty"`
	Middle bool    `json:"middle,omitempty"`
}

// ScrollInput is wheel notches at the cursor: positive DY scrolls the
// content down into view from above (wheel up), positive DX from the left.
type ScrollInput struct {
	DX int `json:"dx"`
	DY int `json:"dy"`
}

// SwipePhase is a space swipe's step.
type SwipePhase string

const (
	SwipeBegan     SwipePhase = "began"
	SwipeChanged   SwipePhase = "changed"
	SwipeEnded     SwipePhase = "ended"
	SwipeCancelled SwipePhase = "cancelled"
)

func (SwipePhase) Values() []string { return []string{"began", "changed", "ended", "cancelled"} }

// SpaceSwipeInput is a step of a live swipe between a Mac's spaces.
// Progress is in spaces; an end's direction is -1 or 1 to finish the move
// that way, 0 or absent to slide back.
type SpaceSwipeInput struct {
	Phase     SwipePhase `json:"phase"`
	Progress  float64    `json:"progress"`
	Direction *int       `json:"direction,omitempty"`
}

// KeyframeInput asks for a keyframe at once, after the phone's decoder failed.
type KeyframeInput struct{}

// MicInput makes the phone's microphone an input on the Mac, or stops it.
type MicInput struct {
	On bool `json:"on"`
}

// MicAudioInput is the phone's microphone while it is on: 16-bit signed
// little-endian mono PCM at 24 kHz, a few tens of milliseconds at a time.
type MicAudioInput struct {
	PCM []byte `json:"pcm"`
}

// SoundInput sends the screen's sound to the phone, or stops it. While it
// is on, the Mac's own speakers are quiet.
type SoundInput struct {
	On bool `json:"on"`
}

// The frame header: 16 bytes in front of every frame, big-endian. Byte 0
// is its size, so fields can be added at its end; 1 the codec; 2 the
// flags; 3 the quarter turns clockwise the phone turns the frame; 4–7 a
// sequence number; 8–15 the capture time in microseconds on the host's
// uptime clock, 0 when the source gives none.
const (
	frameHeaderSize = 16
	frameCodecH264  = 1
	frameCodecAAC   = 3
	// Flags.
	frameKeyframe      = 1 << 0
	frameParameterSets = 1 << 1
)

// negotiate picks the version and codec, or says why none fits.
func negotiate(req StreamRequest) (StreamReady, error) {
	version := 0
	for _, v := range req.Versions {
		if v <= StreamVersion && v > version {
			version = v
		}
	}
	if version == 0 {
		return StreamReady{}, newError(errkind.Invalid, ErrorUnsupportedVersion,
			fmt.Sprintf("This environment speaks screen stream version %d. Update RepoGo.", StreamVersion))
	}
	for _, c := range req.Codecs {
		if c == CodecH264 {
			return StreamReady{Version: version, Codec: CodecH264}, nil
		}
	}
	return StreamReady{}, newError(errkind.Invalid, ErrorUnsupportedVersion, "This app can't decode H.264 video.")
}

// Stream serves a /stream socket: its request, then the screen as frames
// until ctx ends, playing the phone's input on it. The socket sends an
// error returned as the failed message, with its code (StreamCode).
func (m *Manager) Stream(ctx context.Context, req StreamRequest, phone Phone, send func([]byte) error,
	reply func(any) error, input <-chan StreamInput) error {
	ready, err := negotiate(req)
	if err != nil {
		return err
	}
	c, err := m.current(ctx, req.Screen)
	if err != nil {
		return err
	}
	sayReady := func() error { return reply(StreamMessage{Type: "ready", Ready: &ready}) }
	queue := newInputQueue()
	// Each stream's bitrate starts at its kind of screen's start: one
	// learned on 3G and kept would start fast Wi-Fi at 0.5 Mbps.
	pace := newPacer()
	meter := newMeter(pace)
	framed := &framer{c: c, pace: pace, meter: meter, send: send}
	go reportStats(ctx, meter, reply)
	sound := &soundOut{c: c, framed: framed, reply: reply}
	mic := &micIn{c: c, phone: phone, reply: reply}
	// The sound leaves out the mic's own playing, so the phone doesn't hear itself.
	sound.exclude = mic.process
	mic.changed = func() { sound.restart(ctx) }
	// The sound first: the mic stopping restarts a sound still running.
	defer mic.stop()
	defer sound.stop()
	var edges *edgeWatch
	if c.d.Kind == KindDisplay {
		edges = &edgeWatch{ctx: ctx, reply: reply, read: macScrollEdge()}
	}
	go func() {
		defer queue.close()
		for in := range input {
			switch {
			case in.Ack != nil:
				pace.ack(in.Ack.Bytes)
				continue
			case in.Ping != nil:
				// Answered here, ahead of the queued input, so the round
				// trip doesn't count the device's work.
				_ = reply(StreamMessage{Type: "pong", Pong: &StreamPong{ID: in.Ping.ID, HostUS: uptimeUS()}})
				continue
			}
			edges.saw(in)
			switch {
			case in.Sound != nil:
				sound.set(ctx, in.Sound.On)
				continue
			case in.Mic != nil:
				mic.set(ctx, in.Mic.On)
				continue
			case in.MicAudio != nil:
				mic.write(in.MicAudio.PCM)
				continue
			}
			for _, it := range c.translate(in) {
				queue.push(it)
			}
		}
	}()
	switch c.d.Kind {
	case KindDisplay:
		// A display says ready itself, once Screen Sharing took the password.
		err = streamDisplay(ctx, c, req.Password, sayReady, framed, queue)
	case KindSimulator:
		if err = sayReady(); err == nil {
			err = streamIOS(ctx, c, framed, queue)
		}
	default:
		if err = sayReady(); err == nil {
			err = streamAndroid(ctx, c, framed, queue)
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// Phone is the paired phone on a stream: its device ID and what it's called.
type Phone struct {
	ID   string
	Name string
}

// item is one input after translation: for the device's input path, or a
// step the host carries out itself, in order with the rest.
type item struct {
	device capture.Input
	host   func(ctx context.Context, device func(capture.Input) error) error
}

// translate turns a phone's input into what the device takes.
func (c *controller) translate(in StreamInput) []item {
	dev := func(in capture.Input) []item { return []item{{device: in}} }
	switch {
	case in.Touch != nil:
		t := in.Touch
		// The devices take one finger; another's touches would fight the first.
		if t.Finger != 0 || t.X < 0 || t.X > 1 || t.Y < 0 || t.Y > 1 {
			return nil
		}
		switch t.Phase {
		case TouchDown, TouchMove, TouchUp, TouchCancel:
			return dev(capture.Input{Touch: string(t.Phase), X: t.X, Y: t.Y})
		}
	case in.Button != nil:
		return c.button(in.Button.Button)
	case in.Text != nil && in.Text.Text != "":
		text := in.Text.Text
		if c.d.Platform == PlatformIOS {
			return []item{{host: func(ctx context.Context, device func(capture.Input) error) error {
				return c.typeIOS(ctx, text, device)
			}}}
		}
		return dev(capture.Input{Action: capture.ActionText, Value: text})
	case in.Key != nil:
		if in.Key.Keysym <= 0 || in.Key.Keysym > 0x1fffffff {
			return nil
		}
		return dev(capture.Input{Key: &capture.Key{Keysym: uint32(in.Key.Keysym), Down: in.Key.Down}})
	case in.Pointer != nil:
		p := in.Pointer
		if p.X < 0 || p.X > 1 || p.Y < 0 || p.Y > 1 {
			return nil
		}
		var buttons byte
		if p.Left {
			buttons |= 1
		}
		if p.Middle {
			buttons |= 2
		}
		if p.Right {
			buttons |= 4
		}
		return dev(capture.Input{Pointer: &capture.Pointer{X: p.X, Y: p.Y, Buttons: buttons}})
	case in.Scroll != nil:
		return dev(capture.Input{Scroll: &capture.Scroll{DX: in.Scroll.DX, DY: in.Scroll.DY}})
	case in.SpaceSwipe != nil && c.d.Kind == KindDisplay:
		step := *in.SpaceSwipe
		return []item{{host: func(ctx context.Context, device func(capture.Input) error) error {
			return c.swipe(ctx, step, device)
		}}}
	case in.Keyframe != nil:
		return dev(capture.Input{Action: capture.ActionKeyframe})
	}
	return nil
}

// button is a hardware button's press on this device.
func (c *controller) button(b HardwareButton) []item {
	if !slices.Contains(c.buttons(), b) {
		return nil
	}
	switch {
	case b == ButtonAppSwitcher && c.d.Platform == PlatformIOS:
		// Home pressed twice where iOS still takes Indigo's button; from
		// iOS 26, which drops it, Home twice through dtuhidd, or the swipe up
		// from the upright screen's bottom where the simulator has none.
		in := capture.Input{Action: capture.ActionAppSwitcher, Value: capture.SwitcherPress}
		if iosMajor(c.d.OS.Version) >= 26 {
			in.Value = ""
		}
		return []item{{device: in}}
	case b == ButtonHome && c.d.Platform == PlatformIOS && iosMajor(c.d.OS.Version) >= 26:
		// iOS 26 and later drop the Home button's HID press.
		return []item{{host: func(ctx context.Context, _ func(capture.Input) error) error {
			_, err := c.simctl(ctx, "launch", c.d.ID, "com.apple.springboard")
			return err
		}}}
	}
	return []item{{device: capture.Input{Button: string(b)}}}
}

// inputQueue holds the phone's input until the device takes it. It never
// drops a press, a lift or a key: under pressure only a finger's or the
// cursor's moves fold into the latest.
type inputQueue struct {
	mu    sync.Mutex
	items []item
	// buttons are the cursor's buttons as the last pointer queued left them.
	buttons byte
	closed  bool
	wake    chan struct{}
}

// maxQueued bounds the moves held; presses and keys are kept past it.
const maxQueued = 512

func newInputQueue() *inputQueue { return &inputQueue{wake: make(chan struct{}, 1)} }

func (q *inputQueue) push(it item) {
	q.mu.Lock()
	if n := len(q.items); n > 0 && movesOn(q.items[n-1], it) {
		q.items[n-1] = it
	} else if n < maxQueued || !isMove(it, q.buttons) {
		q.items = append(q.items, it)
	}
	if p := it.device.Pointer; it.host == nil && p != nil {
		q.buttons = p.Buttons
	}
	q.mu.Unlock()
	q.signal()
}

func (q *inputQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// isMove reports whether it can be dropped: the finger moving, or the cursor
// moving with the buttons it already held. A pointer that presses or lets go
// of a button is kept.
func isMove(it item, buttons byte) bool {
	d := it.device
	if it.host != nil {
		return false
	}
	return d.Touch == "move" || (d.Touch == "" && d.Pointer != nil && d.Pointer.Buttons == buttons)
}

// movesOn reports whether next only moves on from last: the finger moving
// again, or the cursor moving with the same buttons held.
func movesOn(last, next item) bool {
	if last.host != nil || next.host != nil {
		return false
	}
	a, b := last.device, next.device
	if a.Touch == "move" && b.Touch == "move" {
		return true
	}
	return a.Pointer != nil && b.Pointer != nil && a.Pointer.Buttons == b.Pointer.Buttons
}

func (q *inputQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

// next is the oldest input, waiting for one; false once closed and empty, or ctx ends.
func (q *inputQueue) next(ctx context.Context) (item, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			it := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return it, true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return item{}, false
		}
		select {
		case <-q.wake:
		case <-ctx.Done():
			return item{}, false
		}
	}
}

// serve plays the queue on device, the host's own steps in order with it,
// until the queue ends; then it releases the keys and buttons still held.
// A finger still down is lifted by the device's own path when its input ends.
func (c *controller) serve(ctx context.Context, q *inputQueue, device func(capture.Input) error) {
	keys := map[uint32]bool{}
	var pointer *capture.Pointer
	defer func() {
		for k := range keys {
			device(capture.Input{Key: &capture.Key{Keysym: k}})
		}
		if pointer != nil && pointer.Buttons != 0 {
			device(capture.Input{Pointer: &capture.Pointer{X: pointer.X, Y: pointer.Y}})
		}
	}()
	for {
		it, ok := q.next(ctx)
		if !ok {
			return
		}
		if it.host != nil {
			if err := it.host(ctx, device); err != nil && ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "screens: input:", err)
			}
			continue
		}
		in := it.device
		if in.Key != nil {
			if in.Key.Down {
				keys[in.Key.Keysym] = true
			} else {
				delete(keys, in.Key.Keysym)
			}
		}
		if in.Pointer != nil {
			p := *in.Pointer
			pointer = &p
		}
		if device(in) != nil {
			return
		}
	}
}

// framer puts the header on each frame it sends.
type framer struct {
	c             *controller
	pace          *pacer
	meter         *meter
	send          func([]byte) error
	mu            sync.Mutex
	sequence      uint32
	audioSequence uint32
}

// child sends one frame from a capture child: its capture time (8 bytes,
// big-endian microseconds), then the access unit.
func (f *framer) child(frame []byte) error {
	if len(frame) < 8 {
		return nil
	}
	return f.frame(frame[8:], binary.BigEndian.Uint64(frame[:8]))
}

// reportStats sends the meter's stats every statsEvery until ctx ends.
func reportStats(ctx context.Context, m *meter, reply func(any) error) {
	ticker := time.NewTicker(statsEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			stats, ok := m.report()
			m.pace.tick(stats.Frames, time.Now())
			if ok {
				_ = reply(StreamMessage{Type: "stats", Stats: &stats})
			}
		}
	}
}

// frame sends an access unit, NAL units behind 4-byte lengths, with its header.
func (f *framer) frame(unit []byte, capturedUS uint64) error {
	f.mu.Lock()
	sequence := f.sequence
	f.sequence++
	f.mu.Unlock()
	out := make([]byte, frameHeaderSize, frameHeaderSize+len(unit))
	out[0] = frameHeaderSize
	out[1] = frameCodecH264
	out[2] = nalFlags(unit)
	out[3] = 0 // no turn: frames go out upright (controller.rotation)
	binary.BigEndian.PutUint32(out[4:], sequence)
	binary.BigEndian.PutUint64(out[8:], capturedUS)
	out = append(out, unit...)
	if err := f.send(out); err != nil {
		return err
	}
	if f.pace != nil {
		f.pace.wrote(len(out), true)
		f.meter.video(capturedUS)
	}
	return nil
}

// audio sends one AAC access unit with its header: codec AAC, no flags, no
// turn, a sequence of its own.
func (f *framer) audio(frame []byte) error {
	if len(frame) < 8 {
		return nil
	}
	f.mu.Lock()
	sequence := f.audioSequence
	f.audioSequence++
	f.mu.Unlock()
	out := make([]byte, frameHeaderSize, frameHeaderSize+len(frame)-8)
	out[0] = frameHeaderSize
	out[1] = frameCodecAAC
	binary.BigEndian.PutUint32(out[4:], sequence)
	copy(out[8:16], frame[:8])
	out = append(out, frame[8:]...)
	if err := f.send(out); err != nil {
		return err
	}
	if f.pace != nil {
		f.pace.wrote(len(out), false)
	}
	return nil
}

// nalFlags reads an access unit's NAL types: an IDR slice makes it a
// keyframe, an SPS or PPS gives it the parameter sets.
func nalFlags(unit []byte) byte {
	var flags byte
	for len(unit) >= 5 {
		n := int(binary.BigEndian.Uint32(unit))
		if n <= 0 || n > len(unit)-4 {
			break
		}
		switch unit[4] & 0x1f {
		case 5:
			flags |= frameKeyframe
		case 7, 8:
			flags |= frameParameterSets
		}
		unit = unit[4+n:]
	}
	return flags
}

// childWait is how long an input child has, once its view ends, to play
// what it was sent and exit before it is killed.
const childWait = 3 * time.Second

// endsWithInput lets cmd, made with ctx, outlive it: the child exits when
// its input closes, after the release written there reached it, and is
// killed only if it is still running childWait later.
func endsWithInput(cmd *exec.Cmd) {
	cmd.Cancel = func() error { return nil }
	cmd.WaitDelay = childWait
}

// streamChild runs this binary again as args (simcap, screencap,
// androidcap), passing first and then the input to it as lines, and its
// framed output on as frames.
func streamChild(ctx context.Context, c *controller, send func([]byte) error, pace *pacer, input *inputQueue,
	args []string, first []byte) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(run, exe, args...)
	endsWithInput(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr tail
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	var mu sync.Mutex
	lines := json.NewEncoder(stdin)
	toChild := func(in capture.Input) error {
		mu.Lock()
		defer mu.Unlock()
		return lines.Encode(in)
	}
	defer c.open(toChild)()
	served := make(chan struct{})
	go func() {
		defer close(served)
		defer stdin.Close()
		if first != nil {
			if _, err := stdin.Write(append(first, '\n')); err != nil {
				return
			}
		}
		// The pacer's credit goes straight to the child, after its first
		// line and not through the input queue, so a slow step there
		// (typing on iOS) never holds frames back.
		if pace != nil {
			defer pace.attach(toChild, bitrates(c.d.Kind))()
		}
		c.serve(run, input, toChild)
	}()
	err = readFrames(out, send, c.fromSimcap)
	// The view is over: serve releases what is held, then the child's input
	// closes and it exits.
	cancel()
	<-served
	go io.Copy(io.Discard, out)
	cmd.Wait()
	if ctx.Err() != nil {
		return nil
	}
	if why := stderr.String(); why != "" {
		return errors.New(why)
	}
	if err == nil {
		err = errors.New("the live view stopped")
	}
	return err
}

// pump runs cmd, whose stdout is Annex B H.264, sending each access unit
// through framed until it exits, ctx ends or the send fails. cmd must stop with ctx.
func pump(ctx context.Context, cmd *exec.Cmd, framed *framer) error {
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Wait()
	chunks := make(chan []byte)
	go readChunks(ctx, out, chunks)

	var units accessUnits
	idle := time.NewTimer(time.Hour)
	defer idle.Stop()
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				return nil
			}
			for _, unit := range units.write(chunk) {
				// screenrecord gives no capture time.
				if err := framed.frame(unit, 0); err != nil {
					return err
				}
			}
			idle.Reset(idleFlush)
		case <-idle.C:
			for _, unit := range units.flush() {
				if err := framed.frame(unit, 0); err != nil {
					return err
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// maxFrame bounds one child frame, so a corrupt length can't take memory;
// hostMessage marks a length whose message is for the host, not the phone.
const (
	maxFrame    = 16 << 20
	hostMessage = 1 << 31
)

// readFrames reads out, a capture child's framed messages, passing each
// frame on as it arrives until out ends or frame fails. A message with its
// length's top bit set is for the host: said.
func readFrames(out io.Reader, frame func([]byte) error, said func([]byte)) error {
	r := bufio.NewReaderSize(out, 256<<10)
	var length [4]byte
	for {
		if _, err := io.ReadFull(r, length[:]); err != nil {
			return nil
		}
		n := binary.BigEndian.Uint32(length[:])
		forHost := n&hostMessage != 0
		n &^= hostMessage
		if n == 0 || n > maxFrame {
			return fmt.Errorf("the capture sent a %d-byte frame", n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil
		}
		if forHost {
			said(buf)
			continue
		}
		if err := frame(buf); err != nil {
			return err
		}
	}
}

// tail keeps the last line a child wrote to stderr: why it failed.
type tail struct {
	mu   sync.Mutex
	last string
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if line := strings.TrimSpace(string(b)); line != "" {
		lines := strings.Split(line, "\n")
		t.last = strings.TrimPrefix(lines[len(lines)-1], "repogo: ")
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.last
}

func readChunks(ctx context.Context, r io.Reader, chunks chan<- []byte) {
	defer close(chunks)
	for {
		b := make([]byte, 64<<10)
		n, err := r.Read(b)
		if n > 0 {
			select {
			case chunks <- b[:n]:
			case <-ctx.Done():
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// accessUnits cuts an Annex B byte stream into access units (frames).
type accessUnits struct {
	pending []byte   // bytes from the last start code on: a NAL unit not known to be whole
	nals    [][]byte // the whole NAL units of the frame being gathered
	hasVCL  bool     // whether nals holds a slice of picture data yet
}

// write adds bytes and returns each frame they complete.
func (a *accessUnits) write(b []byte) [][]byte {
	a.pending = append(a.pending, b...)
	var done [][]byte
	for {
		start, size := startCode(a.pending, 0)
		if start < 0 {
			return done
		}
		next, _ := startCode(a.pending, start+size)
		if next < 0 {
			// Keep from this start code on; what came before it is nothing.
			a.pending = a.pending[start:]
			// A header that opens the next frame already ends this one.
			if nal := a.pending[size:]; a.hasVCL && len(nal) > 1 && (!isVCL(nal) || firstSlice(nal)) {
				done = append(done, a.take())
			}
			return done
		}
		if unit := a.add(a.pending[start+size : next]); unit != nil {
			done = append(done, unit)
		}
		a.pending = a.pending[next:]
	}
}

// flush takes the bytes held as a whole NAL unit and returns the frames that
// ends: the one it closed, if it opened a new one, and the one it is part of.
func (a *accessUnits) flush() [][]byte {
	var done [][]byte
	if start, size := startCode(a.pending, 0); start >= 0 {
		if unit := a.add(a.pending[start+size:]); unit != nil {
			done = append(done, unit)
		}
	}
	a.pending = nil
	if a.hasVCL {
		done = append(done, a.take())
	}
	return done
}

// add files one NAL unit, returning the frame it closed when it opens a new one.
func (a *accessUnits) add(nal []byte) []byte {
	if len(nal) == 0 {
		return nil
	}
	var done []byte
	vcl := isVCL(nal)
	// A parameter set, SEI or delimiter, or the first slice of a picture,
	// begins the next frame once this one has picture data.
	if a.hasVCL && (!vcl || firstSlice(nal)) {
		done = a.take()
	}
	a.nals = append(a.nals, append([]byte(nil), nal...))
	a.hasVCL = a.hasVCL || vcl
	return done
}

// take returns the gathered NAL units length-prefixed, and starts over.
func (a *accessUnits) take() []byte {
	var out []byte
	for _, nal := range a.nals {
		out = binary.BigEndian.AppendUint32(out, uint32(len(nal)))
		out = append(out, nal...)
	}
	a.nals, a.hasVCL = nil, false
	return out
}

// startCode finds 00 00 01 at or after from, counting a zero before it (the
// 4-byte form) into the code; -1 when there is none.
func startCode(b []byte, from int) (at, size int) {
	for i := from; i+3 <= len(b); i++ {
		if b[i] == 0 && b[i+1] == 0 && b[i+2] == 1 {
			if i > from && b[i-1] == 0 {
				return i - 1, 4
			}
			return i, 3
		}
	}
	return -1, 0
}

// isVCL: NAL types 1 to 5 carry picture data.
func isVCL(nal []byte) bool { t := nal[0] & 0x1f; return t >= 1 && t <= 5 }

// firstSlice: the slice header opens with first_mb_in_slice as Exp-Golomb,
// which is 0 exactly when its first bit is 1.
func firstSlice(nal []byte) bool { return len(nal) > 1 && nal[1]&0x80 != 0 }
