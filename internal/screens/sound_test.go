package screens

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/screens/capture"
)

// A sound frame carries the AAC codec, no flags or turn, a sequence of its
// own and the capture time the child stamped, then the unit.
func TestAudioFrameHeader(t *testing.T) {
	c, _ := testController(Screen{Kind: KindDisplay})
	c.rotation = 90
	var sent [][]byte
	f := &framer{c: c, send: func(b []byte) error { sent = append(sent, b); return nil }}
	f.frame(append(binary.BigEndian.AppendUint32(nil, 2), 1, 0x80), 0)
	child := func(us uint64, unit ...byte) []byte { return append(binary.BigEndian.AppendUint64(nil, us), unit...) }
	f.audio(child(4321, 0xaa, 0xbb))
	f.audio(child(5000, 0xcc))
	f.audio([]byte{1, 2, 3}) // shorter than its time: dropped

	if len(sent) != 3 {
		t.Fatalf("sent %d frames, want 3", len(sent))
	}
	a := sent[1]
	if a[0] != 16 || a[1] != frameCodecAAC || a[2] != 0 || a[3] != 0 || binary.BigEndian.Uint32(a[4:]) != 0 ||
		binary.BigEndian.Uint64(a[8:]) != 4321 || string(a[16:]) != "\xaa\xbb" {
		t.Fatalf("first sound frame % x", a)
	}
	if b := sent[2]; binary.BigEndian.Uint32(b[4:]) != 1 {
		t.Fatalf("second sound frame's sequence % x", b[:16])
	}
}

// Each phone's mic has its own ID, the same every time, named after it.
func TestMicDeviceIsThePhones(t *testing.T) {
	uid, name := micDevice(Phone{ID: "phone-a", Name: "Jerrick’s S10+"})
	again, _ := micDevice(Phone{ID: "phone-a", Name: "renamed"})
	other, unnamed := micDevice(Phone{ID: "phone-b"})
	if uid != again || uid == other {
		t.Fatalf("IDs %q %q %q: want one per phone, stable", uid, again, other)
	}
	if !strings.HasPrefix(uid, capture.MicDevicePrefix+".") {
		t.Fatalf("ID %q outside the prefix the host clears at start", uid)
	}
	if name != "RepoGo – Jerrick’s S10+" || unnamed != "RepoGo iPhone Mic" {
		t.Fatalf("names %q, %q", name, unnamed)
	}
}
