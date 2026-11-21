//go:build windows && cgo

package capture

/*
#cgo LDFLAGS: -ld3d11 -ldxgi -lmfplat -lmf -lole32 -loleaut32 -luser32
#include <stdlib.h>
#include "wincap_windows.h"
*/
import "C"

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"unicode/utf16"
)

// RunScreen is `repogo screencap` on Windows: the main monitor, read with
// Desktop Duplication in the signed-in session the host runs in, as frames
// on out like simcap's. Each line of in is an Input: the pointer, wheel
// and keys play through SendInput. No password: the host already runs as
// the user whose screen this is.
func RunScreen(ctx context.Context, out *os.File, in io.Reader) error {
	var why [256]C.char
	// Text is small on a desktop: up to 1080 lines, 8 Mbps, as on a Mac.
	if C.wincap_start(C.uintptr_t(out.Fd()), 1080, 8000000, &why[0], C.int(len(why))) != 0 {
		return errors.New(C.GoString(&why[0]))
	}
	defer C.wincap_stop()
	var pad windowsInput
	// A view that closes mid-press never lets go: release what's held.
	defer pad.release()
	lines := bufio.NewScanner(in)
	for lines.Scan() {
		if ctx.Err() != nil {
			return nil
		}
		var input Input
		if json.Unmarshal(lines.Bytes(), &input) == nil && input.Valid() {
			pad.play(input)
		}
	}
	return lines.Err()
}

// windowsInput plays the phone's input on Windows, keeping the buttons and
// keys held so they can be let go.
type windowsInput struct {
	buttons byte
	keys    map[int]bool
}

// pointerButtons are Pointer.Buttons' bits, as VNC numbers them: left,
// middle, right.
var pointerButtons = [3]byte{1 << 0, 1 << 1, 1 << 2}

func (p *windowsInput) play(in Input) {
	switch {
	case in.Pointer != nil:
		C.wincap_pointer(C.double(in.Pointer.X), C.double(in.Pointer.Y))
		p.press(in.Pointer.Buttons)
	case in.Scroll != nil:
		// Positive DY brings content down from above: the wheel forward.
		// Positive DX brings it in from the left: the wheel tilted left.
		if in.Scroll.DY != 0 {
			C.wincap_wheel(1, C.int(clampNotches(in.Scroll.DY)*notchDelta))
		}
		if in.Scroll.DX != 0 {
			C.wincap_wheel(0, C.int(-clampNotches(in.Scroll.DX)*notchDelta))
		}
	case in.Key != nil:
		p.key(in.Key.Keysym, in.Key.Down)
	case in.Touch != "":
		// Direct touch: the left button held while the finger is down.
		C.wincap_pointer(C.double(in.X), C.double(in.Y))
		var buttons byte
		if in.Touch == "down" || in.Touch == "move" {
			buttons = pointerButtons[0]
		}
		p.press(buttons)
	case in.Action == ActionKeyframe:
		C.wincap_keyframe()
	case in.Credit > 0 || in.Bitrate > 0:
		if in.Credit > 0 {
			C.wincap_credit(C.int(in.Credit))
		}
		if in.Bitrate > 0 {
			C.wincap_bitrate(C.int(in.Bitrate))
		}
	case in.Action == ActionText:
		for _, unit := range utf16.Encode([]rune(in.Value)) {
			if unit == '\n' {
				C.wincap_vk(C.int(vkReturn), 1, 0)
				C.wincap_vk(C.int(vkReturn), 0, 0)
				continue
			}
			C.wincap_unicode(C.uint16_t(unit))
		}
	}
}

func clampNotches(n int) int { return max(-20, min(20, n)) }

// notchDelta is a phone notch as wheel travel: a line, as a notch scrolls
// on a Mac. The phone counts a notch every point or so of a finger's
// travel, and a whole click (120) scrolls three lines on Windows, which
// ran three times as fast with no steps between. Apps that scroll smoothly
// move by each part; older ones add them up to whole clicks.
const notchDelta = 40

// press moves each mouse button to where buttons has it.
func (p *windowsInput) press(buttons byte) {
	for i, bit := range pointerButtons {
		if p.buttons&bit != buttons&bit {
			down := 0
			if buttons&bit != 0 {
				down = 1
			}
			C.wincap_button(C.int(i), C.int(down))
		}
	}
	p.buttons = buttons
}

// key presses or releases the key a keysym names. The phone names a
// character (@, A) with no Shift of its own: one this keyboard types with
// Shift gets Shift around it, unless the phone's Shift is already held.
// One that needs Ctrl or Alt here (AltGr on some layouts), or has no key,
// is typed as the character itself.
func (p *windowsInput) key(keysym uint32, down bool) {
	vk, extended, shifted, ok := vkFor(keysym)
	if !ok {
		if down && keysym >= 0x20 && keysym <= 0xffff {
			C.wincap_unicode(C.uint16_t(keysym))
		}
		return
	}
	if shifted && !p.keys[vkShift] {
		if down {
			C.wincap_vk(vkShift, 1, 0)
			C.wincap_vk(C.int(vk), 1, 0)
			C.wincap_vk(C.int(vk), 0, 0)
			C.wincap_vk(vkShift, 0, 0)
		}
		return
	}
	if p.keys == nil {
		p.keys = map[int]bool{}
	}
	p.keys[vk] = down
	C.wincap_vk(C.int(vk), boolInt(down), boolInt(extended))
}

func (p *windowsInput) release() {
	p.press(0)
	for vk, down := range p.keys {
		if down {
			_, extended := extendedKeys[vk]
			C.wincap_vk(C.int(vk), 0, boolInt(extended))
		}
	}
}

func boolInt(b bool) C.int {
	if b {
		return 1
	}
	return 0
}

// Virtual-key codes.
const (
	vkBack    = 0x08
	vkTab     = 0x09
	vkReturn  = 0x0D
	vkShift   = 0x10
	vkControl = 0x11
	vkMenu    = 0x12 // Alt
	vkEscape  = 0x1B
	vkPrior   = 0x21
	vkNext    = 0x22
	vkEnd     = 0x23
	vkHome    = 0x24
	vkLeft    = 0x25
	vkUp      = 0x26
	vkRight   = 0x27
	vkDown    = 0x28
	vkDelete  = 0x2E
	vkF1      = 0x70
)

// windowsKeys are the virtual keys of the keysyms a phone keyboard sends
// for keys without a character. ⌘ is Ctrl, so the phone's shortcuts (⌘C,
// ⌘V, ⌘Z) do what they do on a Mac.
var windowsKeys = map[uint32]int{
	0xff08: vkBack,
	0xff09: vkTab,
	0xff0d: vkReturn,
	0xff1b: vkEscape,
	0xffff: vkDelete,
	0xff50: vkHome,
	0xff51: vkLeft,
	0xff52: vkUp,
	0xff53: vkRight,
	0xff54: vkDown,
	0xff55: vkPrior,
	0xff56: vkNext,
	0xff57: vkEnd,
	0xffe1: vkShift,
	0xffe3: vkControl,
	0xffe9: vkMenu,
	0xffeb: vkControl,
}

// extendedKeys need KEYEVENTF_EXTENDEDKEY, or the number pad's take them.
var extendedKeys = map[int]struct{}{
	vkPrior: {}, vkNext: {}, vkEnd: {}, vkHome: {}, vkLeft: {}, vkUp: {}, vkRight: {}, vkDown: {}, vkDelete: {},
}

// vkFor is the virtual key a keysym presses, whether it's an extended key,
// whether this keyboard types it with Shift, and whether it has a key for
// it without Ctrl or Alt.
func vkFor(keysym uint32) (vk int, extended, shifted, ok bool) {
	if vk, ok := windowsKeys[keysym]; ok {
		_, extended := extendedKeys[vk]
		return vk, extended, false, true
	}
	if keysym >= 0xffbe && keysym <= 0xffc9 { // F1 to F12
		return vkF1 + int(keysym-0xffbe), false, false, true
	}
	if keysym < 0x20 || keysym > 0x7e {
		return 0, false, false, false
	}
	// VkKeyScanW: the key in the low byte; in the high one the modifiers
	// it needs, 1 Shift, 2 Ctrl, 4 Alt.
	scan := int(C.wincap_vk_scan(C.uint16_t(keysym)))
	if scan == -1 || scan&0xff == 0xff || scan>>8&6 != 0 {
		return 0, false, false, false
	}
	return scan & 0xff, false, scan>>8&1 != 0, true
}
