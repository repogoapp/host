package screens

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/repogoapp/host/internal/screens/capture"
)

// micDevice names a phone's mic on the Mac, and gives it an ID that stays
// the same for that phone, so an app that picked it picks it again.
func micDevice(phone Phone) (uid, name string) {
	sum := sha256.Sum256([]byte(phone.ID))
	uid = capture.MicDevicePrefix + "." + hex.EncodeToString(sum[:6])
	name = "RepoGo iPhone Mic"
	if label := strings.TrimSpace(phone.Name); label != "" {
		name = "RepoGo – " + label
	}
	return uid, name
}

// micIn makes the phone's microphone an input on the Mac while the phone
// asks: a child (`repogo miccap`) plays the PCM the phone sends, muted,
// and taps it into a public device apps can pick. Only a display's stream
// has it; it ends with the stream, and the device with the child.
type micIn struct {
	c     *controller
	phone Phone
	reply func(any) error
	// changed runs when the child's process starts or stops.
	changed func()

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	stdin  io.WriteCloser
	pid    int
}

// set starts or stops the mic, saying which on the stream.
func (m *micIn) set(ctx context.Context, on bool) {
	if !on {
		m.stop()
		m.say(StreamMic{On: false})
		return
	}
	if m.c.d.Kind != KindDisplay {
		m.say(StreamMic{Error: "The iPhone mic works only with a Mac's screen."})
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		return
	}
	run, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.done = make(chan struct{})
	go func(done chan struct{}) {
		defer close(done)
		err := m.run(run)
		m.mu.Lock()
		stopped := run.Err() != nil
		if !stopped {
			m.cancel, m.stdin, m.pid = nil, nil, 0
		}
		m.mu.Unlock()
		if stopped {
			return // stopped on purpose, or the stream ended
		}
		m.changed()
		why := "The iPhone mic stopped."
		if err != nil {
			why = err.Error()
		}
		m.say(StreamMic{Error: why})
	}(m.done)
}

// write passes one chunk of the phone's PCM to the child; dropped while
// the mic is off.
func (m *micIn) write(pcm []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stdin == nil || len(pcm) == 0 {
		return
	}
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(pcm)))
	m.stdin.Write(append(length[:], pcm...))
}

// process is the child's process ID while it runs, else 0.
func (m *micIn) process() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pid
}

// stop ends the mic if it runs, waiting for the child, so the device is
// gone when it returns.
func (m *micIn) stop() {
	m.mu.Lock()
	cancel, done, stdin, had := m.cancel, m.done, m.stdin, m.pid != 0
	m.cancel, m.done, m.stdin, m.pid = nil, nil, nil, 0
	m.mu.Unlock()
	if cancel == nil {
		return
	}
	// Closing its input lets the child remove the device before it exits.
	if stdin != nil {
		stdin.Close()
	}
	cancel()
	<-done
	if had {
		m.changed()
	}
}

func (m *micIn) say(mic StreamMic) {
	m.reply(StreamMessage{Type: "mic", Mic: &mic})
}

// run runs the child until ctx ends or it stops.
func (m *micIn) run(ctx context.Context) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	uid, name := micDevice(m.phone)
	cmd := exec.CommandContext(ctx, exe, "miccap", uid, name)
	// Its input closing ends it, removing the device; a kill is the fallback.
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
	readFrames(out, func([]byte) error { return nil }, func(message []byte) {
		var said struct {
			Mic *struct {
				PID int `json:"pid"`
			} `json:"mic"`
		}
		if json.Unmarshal(message, &said) != nil || said.Mic == nil {
			return
		}
		m.mu.Lock()
		if m.cancel == nil || ctx.Err() != nil {
			m.mu.Unlock()
			return
		}
		m.stdin, m.pid = stdin, said.Mic.PID
		m.mu.Unlock()
		m.changed()
		m.say(StreamMic{On: true, Device: name})
	})
	stdin.Close()
	cmd.Wait()
	if why := stderr.String(); why != "" && ctx.Err() == nil {
		return errors.New(why)
	}
	return nil
}
