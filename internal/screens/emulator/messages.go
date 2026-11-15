package emulator

import (
	"encoding/binary"
	"errors"
)

// The emulator's messages, encoded by hand: a few fields each, from
// emulator_controller.proto in the Android SDK's emulator/lib.

// Frame is one display frame: RGBA, 4 bytes a pixel, rows packed.
type Frame struct {
	Width, Height int
	Pixels        []byte
	Seq           uint32
	// Turns is the device's physical turn the frame is drawn at, in quarter
	// turns (0 portrait, 1 landscape, 2 upside down, 3 the other landscape):
	// the emulator draws its display as someone holding it would see it.
	Turns int
}

// Formats for ImageFormat.format.
const formatRGBA8888 = 1

// ScreenFormat asks for RGBA frames of the main display, scaled to fit
// width by height (0 for the display's own size).
func ScreenFormat(width, height int) []byte {
	var m []byte
	m = appendVarint(m, 1, formatRGBA8888)
	if width > 0 {
		m = appendVarint(m, 3, uint64(width))
	}
	if height > 0 {
		m = appendVarint(m, 4, uint64(height))
	}
	return m
}

// ParseImage reads an Image: its format's size and its pixels.
func ParseImage(m []byte) (Frame, error) {
	var f Frame
	err := fields(m, func(num int, value uint64, data []byte) {
		switch num {
		case 1: // format
			fields(data, func(num int, value uint64, inner []byte) {
				switch num {
				case 2: // rotation
					fields(inner, func(num int, value uint64, _ []byte) {
						if num == 1 {
							f.Turns = int(value)
						}
					})
				case 3:
					f.Width = int(value)
				case 4:
					f.Height = int(value)
				}
			})
		case 2: // the older place for width and height
			if f.Width == 0 {
				f.Width = int(value)
			}
		case 3:
			if f.Height == 0 {
				f.Height = int(value)
			}
		case 4:
			f.Pixels = data
		case 5:
			f.Seq = uint32(value)
		}
	})
	return f, err
}

// Touch is one finger: id 0 for the first, pressure 0 lifts it.
type Touch struct {
	X, Y, ID, Pressure int
}

// TouchInput is an InputEvent carrying a TouchEvent of touches.
func TouchInput(touches ...Touch) []byte {
	var event []byte
	for _, t := range touches {
		var touch []byte
		touch = appendVarint(touch, 1, uint64(t.X))
		touch = appendVarint(touch, 2, uint64(t.Y))
		touch = appendVarint(touch, 3, uint64(t.ID))
		touch = appendVarint(touch, 4, uint64(t.Pressure))
		touch = appendVarint(touch, 7, 1) // never expire: a lift ends it
		event = appendBytes(event, 1, touch)
	}
	return appendBytes(nil, 2, event)
}

// Key event types.
const (
	KeyDown  = 0
	KeyUp    = 1
	KeyPress = 2
)

// KeyInput is an InputEvent carrying a KeyboardEvent for a W3C key name
// ("Backspace", "ArrowLeft", "Shift", "a").
func KeyInput(key string, eventType int) []byte {
	var event []byte
	event = appendVarint(event, 2, uint64(eventType))
	event = appendBytes(event, 4, []byte(key))
	return appendBytes(nil, 1, event)
}

// TextInput is an InputEvent typing text, each character pressed and released.
func TextInput(text string) []byte {
	var event []byte
	event = appendVarint(event, 2, KeyPress)
	event = appendBytes(event, 5, []byte(text))
	return appendBytes(nil, 1, event)
}

// MARK: protobuf wire

func appendVarint(b []byte, num int, v uint64) []byte {
	b = binary.AppendUvarint(b, uint64(num)<<3)
	return binary.AppendUvarint(b, v)
}

func appendBytes(b []byte, num int, data []byte) []byte {
	b = binary.AppendUvarint(b, uint64(num)<<3|2)
	b = binary.AppendUvarint(b, uint64(len(data)))
	return append(b, data...)
}

var errWire = errors.New("emulator: malformed message")

// fields calls each with every field of m: a varint's value, or a
// length-delimited field's bytes; fixed-width fields are skipped.
func fields(m []byte, each func(num int, value uint64, data []byte)) error {
	for len(m) > 0 {
		key, n := binary.Uvarint(m)
		if n <= 0 {
			return errWire
		}
		m = m[n:]
		num, kind := int(key>>3), key&7
		switch kind {
		case 0:
			v, n := binary.Uvarint(m)
			if n <= 0 {
				return errWire
			}
			m = m[n:]
			each(num, v, nil)
		case 2:
			size, n := binary.Uvarint(m)
			if n <= 0 || uint64(len(m)-n) < size {
				return errWire
			}
			each(num, 0, m[n:n+int(size)])
			m = m[n+int(size):]
		case 1:
			if len(m) < 8 {
				return errWire
			}
			m = m[8:]
		case 5:
			if len(m) < 4 {
				return errWire
			}
			m = m[4:]
		default:
			return errWire
		}
	}
	return nil
}
