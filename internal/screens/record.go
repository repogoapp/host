package screens

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Recording a simulator's or emulator's screen to a video, for agents and
// `repogo screens recording start`: simctl's recordVideo on iOS (an MP4), the
// emulator's own recorder on Android (a WebM).

// recordLimit ends a recording nobody stopped, so none fills the disk.
const recordLimit = 10 * time.Minute

type recording struct {
	path string
	// end stops the recorder and waits for the file; once.
	end  func() error
	once sync.Once
	err  error
}

func (r *recording) stop() error {
	r.once.Do(func() { r.err = r.end() })
	return r.err
}

// RecordStart starts recording the screen; RecordStop ends it.
func (m *Manager) RecordStart(ctx context.Context, id string) error {
	c, unlock, err := m.lockAgent(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	c.mu.Lock()
	busy, closed := c.agent.recording != nil, c.agent.closed
	c.mu.Unlock()
	if closed {
		return ErrNotRunning
	}
	if busy {
		return errInvalid.Errorf("%s is already recording: stop it first", id)
	}
	ext := ".webm"
	if c.d.Platform == PlatformIOS {
		ext = ".mp4"
		if err := c.knowPanel(ctx); err != nil {
			return err
		}
	}
	path, err := newScreenFile(id, ext)
	if err != nil {
		return err
	}
	var r *recording
	if c.d.Platform == PlatformIOS {
		r, err = c.recordIOS(ctx, path)
	} else {
		r, err = c.recordAndroid(ctx, path)
	}
	if err != nil {
		os.Remove(path)
		return err
	}
	c.change(func() { c.agent.recording = r })
	// Stopped, it stays the screen's recording until RecordStop collects it.
	time.AfterFunc(recordLimit, func() {
		c.agent.lock.Lock()
		defer c.agent.lock.Unlock()
		c.mu.Lock()
		current := c.agent.recording == r
		c.mu.Unlock()
		if current {
			r.stop()
		}
	})
	return nil
}

// RecordStop stops the screen's recording and answers the video's path.
func (m *Manager) RecordStop(ctx context.Context, id string) (string, error) {
	// Held while the recorder finishes, so a start can't begin another meanwhile.
	c, unlock, err := m.lockAgent(ctx, id)
	if err != nil {
		return "", err
	}
	defer unlock()
	c.mu.Lock()
	r := c.agent.recording
	c.mu.Unlock()
	if r == nil {
		return "", errInvalid.Errorf("%s isn't recording", id)
	}
	err = r.stop()
	c.change(func() { c.agent.recording = nil })
	if err != nil {
		return "", err
	}
	return r.path, nil
}

func (c *controller) recordIOS(ctx context.Context, path string) (*recording, error) {
	args := []string{"simctl", "io", c.d.ID, "recordVideo", "--codec=h264", "--force"}
	c.mu.Lock()
	if c.activeScreen != 0 {
		args = append(args, "--display="+strconv.FormatUint(uint64(c.activeScreen), 10))
	}
	c.mu.Unlock()
	// It outlives this call: the stop ends it.
	cmd := exec.Command("xcrun", append(args, path)...)
	out, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = cmd.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	// simctl says when it's recording, or why it can't.
	said := make(chan string, 1)
	exited := make(chan error, 1)
	go func() {
		lines := bufio.NewScanner(out)
		var last string
		for lines.Scan() {
			last = strings.TrimSpace(lines.Text())
			if strings.Contains(last, "Recording started") {
				select {
				case said <- last:
				default:
				}
			}
		}
		exited <- fmt.Errorf("the simulator didn't record: %s", last)
	}()
	waited := make(chan error, 1)
	select {
	case <-said:
	case err := <-exited:
		cmd.Wait()
		return nil, err
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		cmd.Wait()
		return nil, errors.New("the simulator didn't start recording")
	case <-ctx.Done():
		cmd.Process.Kill()
		cmd.Wait()
		return nil, ctx.Err()
	}
	go func() { <-exited; waited <- cmd.Wait() }()
	end := func() error {
		cmd.Process.Signal(os.Interrupt)
		select {
		case err := <-waited:
			if err != nil {
				return fmt.Errorf("the recording didn't finish: %w", err)
			}
			return nil
		case <-time.After(20 * time.Second):
			cmd.Process.Kill()
			return errors.New("the recording didn't finish writing")
		}
	}
	return &recording{path: path, end: end}, nil
}

func (c *controller) recordAndroid(ctx context.Context, path string) (*recording, error) {
	limit := strconv.Itoa(int(recordLimit / time.Second))
	if _, err := run(ctx, c.adb, "-s", c.d.serial, "emu", "screenrecord", "start", "--time-limit", limit, path); err != nil {
		return nil, fmt.Errorf("the emulator didn't record: %w", err)
	}
	end := func() error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, err := run(ctx, c.adb, "-s", c.d.serial, "emu", "screenrecord", "stop"); err != nil {
			return err
		}
		// The emulator finishes the file after it answers: wait until it stops growing.
		var last int64 = -1
		for ctx.Err() == nil {
			time.Sleep(300 * time.Millisecond)
			info, err := os.Stat(path)
			if err == nil && info.Size() > 0 && info.Size() == last {
				return nil
			}
			if err == nil {
				last = info.Size()
			}
		}
		return errors.New("the emulator didn't finish the recording")
	}
	return &recording{path: path, end: end}, nil
}
