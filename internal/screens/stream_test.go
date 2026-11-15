package screens

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// nalus reads a length-prefixed frame back into its NAL units.
func nalus(t *testing.T, frame []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(frame) > 0 {
		if len(frame) < 4 {
			t.Fatalf("short length prefix: % x", frame)
		}
		n := int(binary.BigEndian.Uint32(frame))
		out = append(out, frame[4:4+n])
		frame = frame[4+n:]
	}
	return out
}

var (
	sps    = []byte{0x67, 0x42, 0x00, 0x1f}
	pps    = []byte{0x68, 0xce, 0x3c, 0x80}
	idr    = []byte{0x65, 0x88, 0x84, 0x21} // first slice: top bit of byte 1 set
	slice2 = []byte{0x65, 0x40, 0x11}       // a second slice of the same picture
	pframe = []byte{0x41, 0x9a, 0x02}
)

func annexB(nals ...[]byte) []byte {
	var b []byte
	for i, n := range nals {
		if i%2 == 0 {
			b = append(b, 0, 0, 0, 1)
		} else {
			b = append(b, 0, 0, 1)
		}
		b = append(b, n...)
	}
	return b
}

func TestAccessUnitsGroupSlicesOfOnePicture(t *testing.T) {
	var a accessUnits
	frames := a.write(annexB(sps, pps, idr, slice2, pframe))
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want the keyframe closed by the next picture", len(frames))
	}
	got := nalus(t, frames[0])
	want := [][]byte{sps, pps, idr, slice2}
	if len(got) != len(want) {
		t.Fatalf("keyframe NALs = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("NAL %d = % x, want % x", i, got[i], want[i])
		}
	}
	// The last frame is whole only once the stream goes quiet.
	rest := a.flush()
	if len(rest) != 1 || !bytes.Equal(nalus(t, rest[0])[0], pframe) {
		t.Fatalf("flush = %v, want the P frame", rest)
	}
	if more := a.flush(); len(more) != 0 {
		t.Fatalf("second flush = %v, want nothing", more)
	}
}

func TestAccessUnitsSurviveChunksSplitAnywhere(t *testing.T) {
	stream := annexB(sps, pps, idr, pframe, pframe)
	for cut := 1; cut < len(stream); cut++ {
		var a accessUnits
		frames := append(a.write(stream[:cut]), a.write(stream[cut:])...)
		frames = append(frames, a.flush()...)
		if len(frames) != 3 {
			t.Fatalf("cut at %d: %d frames, want 3", cut, len(frames))
		}
	}
}

func TestFlushHoldsParameterSetsForTheNextPicture(t *testing.T) {
	var a accessUnits
	a.write(annexB(sps, pps))
	if got := a.flush(); len(got) != 0 {
		t.Fatalf("flush without picture data = %v", got)
	}
	a.write(annexB(idr))
	got := a.flush()
	if len(got) != 1 || len(nalus(t, got[0])) != 3 {
		t.Fatalf("keyframe = %v, want sps, pps and the slice", got)
	}
}
