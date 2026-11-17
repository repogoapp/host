package screens

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/screens/capture"
)

// TestMain lets the test binary stand in for a capture child: run as
// `test-child <file>`, it copies its input to file until that ends.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "test-child" {
		f, err := os.Create(os.Args[2])
		if err != nil {
			os.Exit(1)
		}
		io.Copy(f, os.Stdin)
		f.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A view that ends with a key and a button held releases both, and the
// child is left running long enough to take the release.
func TestStreamChildReleasesWhatIsHeldBeforeTheChildGoes(t *testing.T) {
	c, _ := testController(Screen{ID: DisplayID, Kind: KindDisplay, Platform: PlatformMacOS})
	path := filepath.Join(t.TempDir(), "lines")
	q := newInputQueue()
	q.push(item{device: capture.Input{Key: &capture.Key{Keysym: 'a', Down: true}}})
	q.push(item{device: capture.Input{Pointer: &capture.Pointer{X: 0.5, Y: 0.5, Buttons: 1}}})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- streamChild(ctx, c, func([]byte) error { return nil }, q, []string{"test-child", path}, nil)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if b, _ := os.ReadFile(path); strings.Contains(string(b), `"pointer"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never took the input")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The socket closing ends the stream's context with the input still open.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("streamChild = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("streamChild never returned")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []capture.Input
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var in capture.Input
		if err := json.Unmarshal([]byte(line), &in); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		got = append(got, in)
	}
	if len(got) != 4 || got[2].Key == nil || got[2].Key.Keysym != 'a' || got[2].Key.Down ||
		got[3].Pointer == nil || got[3].Pointer.Buttons != 0 {
		t.Fatalf("the child took %s, want the key and the button released at the end", b)
	}
}

// Past its bound the queue drops the cursor's moves, never a pointer that
// presses or lets go of a button.
func TestInputQueueKeepsButtonChangesPastItsBound(t *testing.T) {
	q := newInputQueue()
	key := item{device: capture.Input{Key: &capture.Key{Keysym: 'a', Down: true}}}
	pointer := func(x float64, buttons byte) item {
		return item{device: capture.Input{Pointer: &capture.Pointer{X: x, Buttons: buttons}}}
	}
	for range maxQueued {
		q.push(key)
	}
	q.push(pointer(0.1, 0)) // a move: dropped
	q.push(pointer(0.2, 1)) // the press
	q.push(key)
	q.push(pointer(0.3, 1)) // a drag: dropped
	q.push(pointer(0.4, 0)) // the release
	q.close()
	var got []capture.Pointer
	for {
		it, ok := q.next(t.Context())
		if !ok {
			break
		}
		if p := it.device.Pointer; p != nil {
			got = append(got, *p)
		}
	}
	if len(got) != 2 || got[0] != (capture.Pointer{X: 0.2, Buttons: 1}) || got[1] != (capture.Pointer{X: 0.4}) {
		t.Fatalf("pointers %+v, want the press and the release", got)
	}
}

// A recorder started again goes on counting where the last one stopped.
func TestPumpKeepsTheSequenceAcrossRuns(t *testing.T) {
	c, _ := testController(Screen{Kind: KindEmulator, Platform: PlatformAndroid})
	path := filepath.Join(t.TempDir(), "h264")
	// The P frame closes the keyframe; it stays open itself when the run ends.
	if err := os.WriteFile(path, annexB(sps, pps, idr, pframe), 0o644); err != nil {
		t.Fatal(err)
	}
	var sequences []uint32
	framed := &framer{c: c, send: func(b []byte) error {
		sequences = append(sequences, binary.BigEndian.Uint32(b[4:]))
		return nil
	}}
	for range 2 {
		if err := pump(t.Context(), exec.Command("cat", path), framed); err != nil {
			t.Fatal(err)
		}
	}
	if len(sequences) != 2 || sequences[0] != 0 || sequences[1] != 1 {
		t.Fatalf("sequences %v, want 0 then 1", sequences)
	}
}

// A boot and a shutdown of one screen take turns; another screen's go ahead.
func TestTransitionsTakeTurnsPerScreen(t *testing.T) {
	set := transitionSet{m: map[string]ScreenState{}, turns: map[string]chan struct{}{}}
	end, err := set.begin(t.Context(), "A")
	if err != nil {
		t.Fatal(err)
	}
	if other, err := set.begin(t.Context(), "B"); err != nil {
		t.Fatal(err)
	} else {
		other()
	}
	waited, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := set.begin(waited, "A"); err == nil {
		t.Fatal("a second transition began while the first ran")
	}
	end()
	if end, err = set.begin(t.Context(), "A"); err != nil {
		t.Fatalf("after the first ended: %v", err)
	}
	end()
}

// A screenshot no quality fits is made smaller until it does.
func TestAsJPEGShrinksToTheLimit(t *testing.T) {
	noise := image.NewRGBA(image.Rect(0, 0, 256, 256))
	for i := range noise.Pix {
		noise.Pix[i] = byte(rand.IntN(256))
	}
	var shot bytes.Buffer
	if err := png.Encode(&shot, noise); err != nil {
		t.Fatal(err)
	}
	const limit = 4 << 10
	out, err := asJPEG(shot.Bytes(), limit)
	if err != nil || len(out) == 0 || len(out) > limit {
		t.Fatalf("asJPEG = %d bytes, %v; want at most %d", len(out), err, limit)
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil || img.Bounds().Dx() >= 256 {
		t.Fatalf("decoded %v, %v; want a smaller picture", img.Bounds(), err)
	}
}
