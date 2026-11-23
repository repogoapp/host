package screens

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// An emulator's screen by label: axhelper/RepoGoAx.java, run on the device
// as shell, holds a UiAutomation connection and answers each read with the
// screen's elements. One per emulator, while agents read it; it lets go of
// the device's one UiAutomation client after 30 seconds without a read.

//go:embed axhelper/repogo-ax.dex
var axDex []byte

const axDexPath = "/data/local/tmp/repogo-ax.dex"

// axWait bounds the helper's start and each read.
const axWait = 8 * time.Second

type axHelper struct {
	cmd   *exec.Cmd
	stdin io.WriteCloser
	lines chan string
	done  chan struct{}
}

func (c *controller) readAndroidElements(ctx context.Context) (Elements, error) {
	var answer struct {
		Elements []Element `json:"elements"`
		Error    string    `json:"error"`
	}
	for attempt := 0; ; attempt++ {
		if c.agent.android == nil || c.agent.android.exited() {
			h, err := c.startAXHelper(ctx)
			if err != nil {
				return Elements{}, err
			}
			c.agent.android = h
		}
		line, err := c.agent.android.read(ctx)
		if err == nil {
			err = json.Unmarshal([]byte(line), &answer)
		}
		if err == nil {
			break
		}
		// It may have let go while idle as the read went out: once more.
		c.agent.android.close()
		c.agent.android = nil
		if attempt > 0 {
			return Elements{}, err
		}
	}
	if answer.Error != "" {
		return Elements{}, axError(answer.Error)
	}
	w, h := c.screen(ctx)
	// The system's own unlabeled layouts (the status bar's dozen) say nothing.
	kept := answer.Elements[:0]
	for _, e := range answer.Elements {
		if e.Label == "" && e.Value == "" && !strings.Contains(e.Role, "EditText") &&
			(strings.HasPrefix(e.ID, "com.android.systemui:") || strings.HasPrefix(e.ID, "android:id/")) {
			continue
		}
		kept = append(kept, e)
	}
	return Elements{Width: float64(w), Height: float64(h), Unit: "px", Elements: kept}, nil
}

// startAXHelper pushes the helper and starts it on the emulator.
func (c *controller) startAXHelper(ctx context.Context) (*axHelper, error) {
	if c.adb == "" {
		return nil, errors.New("adb isn't installed")
	}
	tmp, err := os.CreateTemp("", "repogo-ax-*.dex")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(axDex)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}
	if _, err := run(ctx, c.adb, "-s", c.d.serial, "push", tmp.Name(), axDexPath); err != nil {
		return nil, fmt.Errorf("couldn't copy the screen reader to the emulator: %w", err)
	}
	cmd := exec.Command(c.adb, "-s", c.d.serial, "shell", "CLASSPATH="+axDexPath+" app_process /system/bin RepoGoAx")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	h := &axHelper{cmd: cmd, stdin: stdin, lines: make(chan string, 1), done: make(chan struct{})}
	go func() {
		r := bufio.NewReaderSize(out, 1<<16)
		for {
			line, err := r.ReadString('\n')
			if line = strings.TrimSpace(line); line != "" {
				h.lines <- line
			}
			if err != nil {
				break
			}
		}
		cmd.Wait()
		close(h.done)
	}()
	first, err := h.next(ctx)
	if err != nil {
		h.close()
		return nil, fmt.Errorf("the emulator's screen reader didn't start: %w", err)
	}
	var said struct {
		Ready bool   `json:"ready"`
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(first), &said); !said.Ready {
		h.close()
		return nil, axError(said.Error)
	}
	return h, nil
}

// axError says what went wrong in the helper, in words for whoever drives it.
func axError(why string) error {
	if strings.Contains(why, "already registered") || strings.Contains(why, "UiAutomation not connected") {
		return errors.New("another tool is reading this emulator's screen (uiautomator, Maestro or Appium): stop it and try again")
	}
	if why == "" {
		why = "it stopped"
	}
	return fmt.Errorf("couldn't read the emulator's screen: %s", why)
}

func (h *axHelper) read(ctx context.Context) (string, error) {
	if _, err := io.WriteString(h.stdin, "read\n"); err != nil {
		return "", err
	}
	return h.next(ctx)
}

func (h *axHelper) next(ctx context.Context) (string, error) {
	select {
	case line := <-h.lines:
		return line, nil
	case <-h.done:
		return "", errors.New("it stopped")
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(axWait):
		return "", errors.New("it didn't answer")
	}
}

func (h *axHelper) exited() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// close ends the helper: its input closing makes it let go and exit.
func (h *axHelper) close() {
	h.stdin.Close()
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		h.cmd.Process.Kill()
	}
}
