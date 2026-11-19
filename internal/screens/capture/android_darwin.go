//go:build darwin && cgo

package capture

/*
#include "simcap_darwin.h"
*/
import "C"

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"math"
	"os"
	"sync/atomic"
	"unsafe"

	"github.com/repogoapp/host/internal/screens/emulator"
)

// RunAndroid is `repogo androidcap`: an Android emulator's display, read
// from its gRPC control port as frames are drawn, encoded on this Mac like
// a simulator's, as frames on out. Touches, keys and buttons from in go
// into the emulator in order, through the same port.
func RunAndroid(ctx context.Context, consolePort int, out *os.File, in io.Reader) error {
	endpoint, err := emulator.Find(consolePort)
	if err != nil {
		return err
	}
	client := emulator.Dial(endpoint)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// A phone's screen, at its own size up to 1080 across, 6 Mbps.
	C.simcap_screen_start(C.int(out.Fd()), 1080, 6000000)

	// The last frame's size and turn, which touches are mapped through.
	var shape atomic.Pointer[frameShape]
	events := client.Open(ctx, "streamInputEvent")
	go func() {
		defer cancel()
		// Closing sends what was written, the lift below too, before the view ends.
		defer events.Close()
		lines := bufio.NewScanner(in)
		var player androidPlayer
		for lines.Scan() {
			var input Input
			if json.Unmarshal(lines.Bytes(), &input) != nil || !input.Valid() {
				continue
			}
			if input.Action == ActionKeyframe {
				C.simcap_keyframe()
				continue
			}
			if input.Credit > 0 || input.Bitrate > 0 {
				pace(input)
				continue
			}
			for _, event := range player.events(input, shape.Load()) {
				if events.Send(event) != nil {
					return
				}
			}
		}
		// A view that closes mid-touch never lifts its finger.
		if player.down {
			events.Send(emulator.TouchInput(emulator.Touch{X: player.x, Y: player.y}))
		}
	}()
	err = client.Stream(ctx, "streamScreenshot", emulator.ScreenFormat(0, 0), func(m []byte) error {
		frame, err := emulator.ParseImage(m)
		// An inactive display sends an empty frame; the last one stays.
		if err != nil || frame.Width == 0 || len(frame.Pixels) < frame.Width*frame.Height*4 {
			return nil
		}
		if s := shape.Load(); s == nil || s.width != frame.Width || s.height != frame.Height || s.turns != frame.Turns {
			shape.Store(&frameShape{width: frame.Width, height: frame.Height, turns: frame.Turns})
		}
		C.simcap_encode_pixels(unsafe.Pointer(&frame.Pixels[0]), C.int(frame.Width), C.int(frame.Height),
			C.int(frame.Width*4), 1)
		return nil
	})
	// The host closing the input ends the view: not a failure.
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// frameShape is a frame's size and the device's physical turn it shows.
type frameShape struct {
	width, height, turns int
}

// panel maps a point on the frame, as fractions of it, to the touch
// panel's own pixels: the emulator draws its display turned as the device
// is held, but takes touches in the panel's upright coordinates (checked
// upside down and turned a quarter, 11-14).
func (s frameShape) panel(fx, fy float64) (int, int) {
	w, h := s.width, s.height
	if s.turns%2 == 1 {
		w, h = h, w
	}
	switch s.turns {
	case 1:
		fx, fy = 1-fy, fx
	case 2:
		fx, fy = 1-fx, 1-fy
	case 3:
		fx, fy = fy, 1-fx
	}
	return int(math.Round(fx * float64(w-1))), int(math.Round(fy * float64(h-1)))
}

// androidPlayer turns the phone's input into the emulator's input events.
type androidPlayer struct {
	x, y int
	down bool
}

func (p *androidPlayer) events(in Input, shape *frameShape) [][]byte {
	var out [][]byte
	if in.Touch != "" && shape != nil {
		x, y := shape.panel(in.X, in.Y)
		pressure := 1
		if in.Touch == "up" || in.Touch == "cancel" {
			pressure, x, y = 0, p.x, p.y
		}
		p.x, p.y, p.down = x, y, pressure == 1
		out = append(out, emulator.TouchInput(emulator.Touch{X: x, Y: y, Pressure: pressure}))
	}
	if key, ok := androidButtons[in.Button]; ok {
		out = append(out, emulator.KeyInput(key, emulator.KeyPress))
	}
	if in.Action == ActionText && in.Value != "" {
		out = append(out, emulator.TextInput(in.Value))
	}
	if in.Key != nil {
		if key := w3cKey(in.Key.Keysym); key != "" {
			kind := emulator.KeyUp
			if in.Key.Down {
				kind = emulator.KeyDown
			}
			out = append(out, emulator.KeyInput(key, kind))
		}
	}
	return out
}

// androidButtons are the device's buttons, by W3C key name.
var androidButtons = map[string]string{
	"home": "GoHome", "back": "GoBack", "recents": "AppSwitch", "lock": "Power",
	"volume_up": "AudioVolumeUp", "volume_down": "AudioVolumeDown",
}

// w3cKey names a keysym's key as the emulator takes it: the special keys
// and modifiers by name, a character as itself.
func w3cKey(keysym uint32) string {
	switch keysym {
	case 0xff08:
		return "Backspace"
	case 0xff09:
		return "Tab"
	case 0xff0d:
		return "Enter"
	case 0xff1b:
		return "Escape"
	case 0xffff:
		return "Delete"
	case 0xff51:
		return "ArrowLeft"
	case 0xff52:
		return "ArrowUp"
	case 0xff53:
		return "ArrowRight"
	case 0xff54:
		return "ArrowDown"
	case 0xffe1:
		return "Shift"
	case 0xffe3:
		return "Control"
	case 0xffe9:
		return "Alt"
	case 0xffeb:
		return "Meta"
	}
	if keysym >= 0x20 && keysym <= 0x7e {
		return string(rune(keysym))
	}
	return ""
}
