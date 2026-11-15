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
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/screens/capture"
)

// Stream sends a running device's screen as H.264 until ctx ends or send
// fails, and plays input on it. Each send is one access unit, NAL units each
// behind a 4-byte length, as AVFoundation reads them. reply sends a Message:
// controls and state first, then what actions report. password is This
// Mac's, from the phone for this session only; empty for every other device.
func Stream(ctx context.Context, id, password string, send func([]byte) error, reply func(any) error,
	input <-chan capture.Input) error {
	d, err := find(ctx, id)
	if err != nil {
		return err
	}
	if !d.Booted {
		return ErrNotRunning
	}
	c := newController(ctx, d, reply)
	reply(Message{Controls: c.controls()})
	c.sendState()
	switch d.Kind {
	case KindSimulator:
		return streamIOS(ctx, c, send, input)
	case KindDisplay:
		return streamDisplay(ctx, c, password, send, input)
	}
	return streamAndroid(ctx, c, send, input)
}

// serve hands each input to the host when it runs it, one at a time, and
// the rest to device (the touch and button path), until input ends.
func (c *controller) serve(ctx context.Context, input <-chan capture.Input, device func(capture.Input) error) {
	actions := make(chan capture.Input, 16)
	defer close(actions)
	go func() {
		for in := range actions {
			if err := c.act(ctx, in, device); err != nil {
				c.reply(Message{Failed: err.Error()})
				// The phone showed the change at once; this puts it back.
				c.sendState()
			}
		}
	}()
	for in := range inputs(ctx, input) {
		if !c.hostRuns(in) {
			if device(in) != nil {
				return
			}
			continue
		}
		select {
		case actions <- in:
		default: // a burst past the queue is dropped rather than held
		}
	}
}

// streamChild runs this binary again as args (simcap, screencap), passing
// first and then input to it as lines, and its framed output on as frames.
func streamChild(ctx context.Context, c *controller, send func([]byte) error, input <-chan capture.Input,
	args []string, first []byte) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var stderr tail
	cmd.Stderr = &stderr
	var mu sync.Mutex
	lines := json.NewEncoder(stdin)
	toSimcap := func(in capture.Input) error {
		mu.Lock()
		defer mu.Unlock()
		return lines.Encode(in)
	}
	go func() {
		defer stdin.Close()
		if first != nil {
			if _, err := stdin.Write(append(first, '\n')); err != nil {
				return
			}
		}
		c.serve(ctx, input, toSimcap)
	}()
	err = pumpFrames(cmd, send, c.fromSimcap)
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

// inputs passes on the valid input until ctx ends or input closes.
func inputs(ctx context.Context, input <-chan capture.Input) <-chan capture.Input {
	out := make(chan capture.Input)
	go func() {
		defer close(out)
		for {
			select {
			case in, ok := <-input:
				if !ok {
					return
				}
				if !in.Valid() {
					continue
				}
				select {
				case out <- in:
				case <-ctx.Done():
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// pump runs cmd, whose stdout is Annex B H.264, sending each access unit
// until it exits, ctx ends or send fails. cmd must stop with ctx.
func pump(ctx context.Context, cmd *exec.Cmd, send func([]byte) error) error {
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
				if err := send(unit); err != nil {
					return err
				}
			}
			idle.Reset(idleFlush)
		case <-idle.C:
			for _, unit := range units.flush() {
				if err := send(unit); err != nil {
					return err
				}
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// maxFrame bounds one simcap frame, so a corrupt length can't take memory;
// hostMessage marks a length whose message is for the host, not the phone.
const (
	maxFrame    = 16 << 20
	hostMessage = 1 << 31
)

// pumpFrames runs cmd, whose stdout is simcap's framed messages, sending each
// frame as it arrives until it exits, ctx ends or send fails. A message with
// its length's top bit set is for the host, and goes to said.
func pumpFrames(cmd *exec.Cmd, send func([]byte) error, said func([]byte)) error {
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer cmd.Wait()
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
			return fmt.Errorf("simcap sent a %d-byte frame", n)
		}
		frame := make([]byte, n)
		if _, err := io.ReadFull(r, frame); err != nil {
			return nil
		}
		if forHost {
			said(frame)
			continue
		}
		if err := send(frame); err != nil {
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
