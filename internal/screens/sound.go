package screens

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"sync"
)

// soundOut sends a display's sound to the phone while the phone asks for
// it: a child (`repogo audiocap mute`) taps everything the Mac plays,
// muting its speakers, and writes AAC frames, sent on as audio frames.
// Only one runs per stream; it ends with the stream.
type soundOut struct {
	c      *controller
	framed *framer
	reply  func(any) error
	// exclude gives a process to leave out of the sound, or 0.
	exclude func() int

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// set starts or stops the sound, saying which on the stream.
func (s *soundOut) set(ctx context.Context, on bool) {
	if !on {
		s.stop()
		s.say(StreamAudio{On: false})
		return
	}
	if s.c.d.Kind != KindDisplay {
		s.say(StreamAudio{Error: "Sound comes only from a Mac's screen for now."})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	run, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.done = make(chan struct{})
	go func(done chan struct{}) {
		defer close(done)
		err := s.run(run)
		if run.Err() != nil {
			return // stopped on purpose, or the stream ended
		}
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
		why := "The sound stopped."
		if err != nil {
			why = err.Error()
		}
		s.say(StreamAudio{Error: why})
	}(s.done)
}

// restart starts the sound again if it runs, so it takes what exclude
// gives now.
func (s *soundOut) restart(ctx context.Context) {
	s.mu.Lock()
	running := s.cancel != nil
	s.mu.Unlock()
	if running {
		s.stop()
		s.set(ctx, true)
	}
}

// stop ends the sound if it runs, waiting for the child, so the Mac's
// speakers are back when it returns.
func (s *soundOut) stop() {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (s *soundOut) say(a StreamAudio) {
	s.reply(StreamMessage{Type: "audio", Audio: &a})
}

// run runs the child until ctx ends or it stops, sending its frames.
func (s *soundOut) run(ctx context.Context) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"audiocap", "mute"}
	if s.exclude != nil {
		if pid := s.exclude(); pid > 0 {
			args = append(args, strconv.Itoa(pid))
		}
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr tail
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	err = readFrames(out, s.framed.audio, func(message []byte) {
		var said struct {
			Audio *struct {
				SampleRate int `json:"sample_rate"`
				Channels   int `json:"channels"`
			} `json:"audio"`
		}
		if json.Unmarshal(message, &said) != nil || said.Audio == nil {
			return
		}
		codec := AudioCodecAAC
		s.say(StreamAudio{On: true, Codec: &codec, SampleRate: &said.Audio.SampleRate, Channels: &said.Audio.Channels})
	})
	cmd.Wait()
	if why := stderr.String(); why != "" && ctx.Err() == nil {
		return errors.New(why)
	}
	return err
}
