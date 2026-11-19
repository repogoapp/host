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
	"errors"
	"io"
	"math"
	"os"
	"os/user"
	"unsafe"

	"github.com/repogoapp/host/internal/screens/vnc"
)

// RunScreen is `repogo screencap`: this Mac's screen, read from Screen
// Sharing on loopback, signed in as the Mac user, as frames on out like
// simcap's. in's first line is {"password":…}, so it is never in an
// environment or a file; touches after it move and click, keys type.
func RunScreen(ctx context.Context, out *os.File, in io.Reader) error {
	account, err := user.Current()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lines := bufio.NewScanner(in)
	var first struct {
		Password string `json:"password"`
	}
	if !lines.Scan() || json.Unmarshal(lines.Bytes(), &first) != nil {
		return errors.New("screencap: no password on the first line")
	}
	client, err := vnc.Dial(ctx, "127.0.0.1:5900", account.Username, first.Password)
	first.Password = ""
	if err != nil {
		return err
	}
	defer client.Close()
	// Text is small on a Mac: up to 1080 lines, 8 Mbps.
	C.simcap_screen_start(C.int(out.Fd()), 1080, 8000000)
	go func() {
		defer cancel()
		var pad screenInput
		for lines.Scan() {
			var input Input
			if json.Unmarshal(lines.Bytes(), &input) == nil && input.Valid() {
				pad.play(client, input)
			}
		}
		// A view that closes mid-touch never lifts its finger.
		if pad.buttons != 0 {
			client.Pointer(pad.x, pad.y, 0)
		}
	}()
	return client.Run(ctx, func() {
		if len(client.Screen) == 0 {
			return
		}
		C.simcap_encode_pixels(unsafe.Pointer(&client.Screen[0]), C.int(client.Width), C.int(client.Height),
			C.int(client.Width*4), 0)
	})
}

// screenInput plays the phone's input on the Mac, keeping the cursor's
// place and held buttons for the wheel, which clicks where the cursor is.
type screenInput struct {
	x, y    int
	buttons byte
}

// Wheel buttons: VNC scrolls by clicking these at the cursor.
const (
	wheelUp, wheelDown, wheelLeft, wheelRight byte = 1 << 3, 1 << 4, 1 << 5, 1 << 6
)

func (s *screenInput) play(c *vnc.Client, in Input) {
	w, h := c.Size()
	at := func(fx, fy float64) (int, int) {
		return int(math.Round(fx * float64(w-1))), int(math.Round(fy * float64(h-1)))
	}
	switch {
	case in.Pointer != nil:
		s.x, s.y = at(in.Pointer.X, in.Pointer.Y)
		s.buttons = in.Pointer.Buttons
		c.Pointer(s.x, s.y, s.buttons)
	case in.Scroll != nil:
		s.wheel(c, in.Scroll.DY, wheelUp, wheelDown)
		s.wheel(c, in.Scroll.DX, wheelLeft, wheelRight)
	case in.Key != nil:
		c.Key(in.Key.Keysym, in.Key.Down)
	case in.Touch != "":
		// Direct touch: the pointer held while the finger is down.
		s.x, s.y = at(in.X, in.Y)
		s.buttons = 0
		if in.Touch == "down" || in.Touch == "move" {
			s.buttons = 1
		}
		c.Pointer(s.x, s.y, s.buttons)
	case in.Action == ActionKeyframe:
		C.simcap_keyframe()
	case in.Credit > 0 || in.Bitrate > 0:
		pace(in)
	case in.Action == ActionText:
		for _, r := range in.Value {
			keysym := Keysym(r)
			c.Key(keysym, true)
			c.Key(keysym, false)
		}
	}
}

// wheel clicks a wheel button once a notch, positive the first, negative the second.
func (s *screenInput) wheel(c *vnc.Client, notches int, positive, negative byte) {
	button := positive
	if notches < 0 {
		button, notches = negative, -notches
	}
	for range min(notches, 20) {
		c.Pointer(s.x, s.y, s.buttons|button)
		c.Pointer(s.x, s.y, s.buttons)
	}
}
