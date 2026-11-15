package emulator

import "testing"

func TestParseImage(t *testing.T) {
	var format []byte
	format = appendVarint(format, 1, formatRGBA8888)
	format = appendBytes(format, 2, appendVarint(nil, 1, 3))
	format = appendVarint(format, 3, 2)
	format = appendVarint(format, 4, 1)
	m := appendBytes(nil, 1, format)
	m = appendBytes(m, 4, []byte{1, 2, 3, 4, 5, 6, 7, 8})
	m = appendVarint(m, 5, 9)
	f, err := ParseImage(m)
	if err != nil || f.Width != 2 || f.Height != 1 || f.Turns != 3 || len(f.Pixels) != 8 || f.Seq != 9 {
		t.Fatalf("ParseImage = %+v, %v", f, err)
	}
	if _, err := ParseImage([]byte{0x0a, 0x7f}); err == nil {
		t.Error("a truncated message parsed")
	}
}

func TestTouchInput(t *testing.T) {
	// InputEvent{touch_event: TouchEvent{touches: [Touch{x: 1, y: 2, pressure: 1, expiration: 1}]}}
	want := []byte{0x12, 0x0c, 0x0a, 0x0a, 0x08, 0x01, 0x10, 0x02, 0x18, 0x00, 0x20, 0x01, 0x38, 0x01}
	if got := TouchInput(Touch{X: 1, Y: 2, Pressure: 1}); string(got) != string(want) {
		t.Errorf("TouchInput = % x, want % x", got, want)
	}
}
