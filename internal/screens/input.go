package screens

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/repogoapp/host/internal/screens/capture"
)

// Agents' input to a screen: on iOS through the newest live view's simcap,
// or a headless one started when none is open; on Android as `input`
// commands over adb.

// headlessView is a simcap child with no frames, kept while agents use it.
type headlessView struct {
	cancel context.CancelFunc
	used   atomic.Int64
}

// headlessIdle is how long an unused headless view stays.
const headlessIdle = 2 * time.Minute

// press runs inputs as a live view would, through one input path;
// c.agent.lock held. Even one that fails partway may have changed the screen.
func (c *controller) press(ctx context.Context, inputs ...StreamInput) error {
	path, err := c.inputPath(ctx)
	if err != nil {
		return err
	}
	defer c.screenChanged()
	for _, in := range inputs {
		if err := c.send(ctx, path, in); err != nil {
			return err
		}
	}
	return nil
}

// send runs one input as a live view would, through path.
func (c *controller) send(ctx context.Context, path func(capture.Input) error, in StreamInput) error {
	items := c.translate(in)
	if len(items) == 0 {
		return ErrUnsupported
	}
	for _, it := range items {
		var err error
		if it.host != nil {
			err = it.host(ctx, path)
		} else {
			err = path(it.device)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// gesture touches down at the first point, moves through the rest over ms,
// and lifts; c.agent.lock held.
func (c *controller) gesture(ctx context.Context, points []Point, ms int) error {
	if c.d.Platform == PlatformAndroid {
		return c.androidGesture(ctx, points, ms)
	}
	size, err := c.screenSize(ctx)
	if err != nil {
		return err
	}
	path, err := c.inputPath(ctx)
	if err != nil {
		return err
	}
	// Every point is checked before the finger goes down, so none is left down.
	for _, p := range points {
		if x, y := p.X/size.X, p.Y/size.Y; x < 0 || x > 1 || y < 0 || y > 1 {
			return errInvalid.Errorf("%.0f, %.0f is off the %.0f×%.0f screen", p.X, p.Y, size.X, size.Y)
		}
	}
	// A finger that went down has changed the screen, whatever happens after.
	defer c.screenChanged()
	at := points[0]
	touch := func(phase TouchPhase, p Point) error {
		at = p
		return c.send(ctx, path, StreamInput{Touch: &TouchInput{Phase: phase, X: p.X / size.X, Y: p.Y / size.Y}})
	}
	if err := touch(TouchDown, points[0]); err != nil {
		return err
	}
	if len(points) == 1 {
		time.Sleep(60 * time.Millisecond)
	} else {
		from, to := points[0], points[len(points)-1]
		steps := max(ms/16, 2)
		for i := 1; i <= steps; i++ {
			time.Sleep(time.Duration(ms/steps) * time.Millisecond)
			f := float64(i) / float64(steps)
			if err := touch(TouchMove, Point{X: from.X + (to.X-from.X)*f, Y: from.Y + (to.Y-from.Y)*f}); err != nil {
				touch(TouchUp, at)
				return err
			}
		}
	}
	return touch(TouchUp, points[len(points)-1])
}

// screenSize is the screen's width and height in the device's units, from
// the last read, or a new one; c.agent.lock held.
func (c *controller) screenSize(ctx context.Context) (Point, error) {
	read := c.lastRead()
	if read == nil {
		r, err := c.readElements(ctx)
		if err != nil {
			return Point{}, err
		}
		read = &r
	}
	if read.Width < 1 || read.Height < 1 {
		return Point{}, errors.New("the screen's size isn't known: is an app open?")
	}
	return Point{X: read.Width, Y: read.Height}, nil
}

// inputPath is where the device's input goes for one action: on iOS the
// newest live view's simcap, or a headless one started for agents; on
// Android the emulator's `input` command.
func (c *controller) inputPath(ctx context.Context) (func(capture.Input) error, error) {
	if c.d.Platform == PlatformAndroid {
		return c.androidPath(ctx), nil
	}
	c.mu.Lock()
	if c.agent.closed {
		c.mu.Unlock()
		return nil, ErrNotRunning
	}
	h := c.agent.headless
	open := len(c.views)
	if open == 0 && h == nil {
		h = c.startHeadless()
	}
	c.mu.Unlock()
	if h != nil {
		h.used.Store(time.Now().UnixNano())
	}
	// The child takes a moment to start, then to say which panel shows
	// (iOS 27): touches before that may go to the wrong one.
	started := time.Now()
	for {
		c.mu.Lock()
		var v *liveView
		if n := len(c.views); n > 0 {
			v = c.views[n-1]
		}
		displayed := c.displayed || !c.universalHID()
		c.mu.Unlock()
		if v != nil && (displayed || time.Since(started) > 2*time.Second) {
			return v.forward, nil
		}
		if time.Since(started) > 8*time.Second {
			return nil, errors.New("couldn't reach the simulator's input")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// startHeadless starts a simcap child with no frames for agents' input; c.mu held.
func (c *controller) startHeadless() *headlessView {
	ctx, cancel := context.WithCancel(context.Background())
	h := &headlessView{cancel: cancel}
	h.used.Store(time.Now().UnixNano())
	c.agent.headless = h
	args := []string{"simcap", c.d.ID}
	if c.universalHID() {
		args = append(args, "universal")
	}
	args = append(args, "headless")
	go func() {
		err := streamChild(ctx, c, func([]byte) error { return nil }, nil, newInputQueue(), args, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "screens: headless input:", err)
		}
		cancel()
		c.mu.Lock()
		if c.agent.headless == h {
			c.agent.headless = nil
		}
		c.mu.Unlock()
	}()
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if time.Since(time.Unix(0, h.used.Load())) > headlessIdle {
					cancel()
					return
				}
			}
		}
	}()
	return h
}

// closeAgent stops what agents started on the screen: it shut down. It
// waits for an action under way, so what that starts is stopped too, and
// nothing starts after.
func (c *controller) closeAgent() {
	c.agent.lock.Lock()
	defer c.agent.lock.Unlock()
	c.mu.Lock()
	c.agent.closed = true
	h, r := c.agent.headless, c.agent.recording
	c.agent.headless, c.agent.recording = nil, nil
	c.screenChangedLocked()
	c.mu.Unlock()
	if r != nil {
		r.stop()
	}
	if h != nil {
		h.cancel()
	}
	if c.agent.android != nil {
		c.agent.android.close()
		c.agent.android = nil
	}
}

// androidPath runs the device input as the emulator's `input` command.
func (c *controller) androidPath(ctx context.Context) func(capture.Input) error {
	w, h := c.screen(ctx)
	return func(in capture.Input) error {
		var lines []string
		switch {
		case in.Action == capture.ActionText:
			lines = androidText(in.Value)
		case in.Key != nil:
			if !in.Key.Down {
				return nil
			}
			if line := androidKey(in.Key.Keysym); line != "" {
				lines = []string{line}
			}
		default:
			if line := androidCommand(in, w, h); line != "" {
				lines = []string{line}
			}
		}
		if len(lines) == 0 {
			return ErrUnsupported
		}
		_, err := c.shell(ctx, strings.Join(lines, "; "))
		return err
	}
}

// androidGesture is a tap or a swipe as one `input` command, in pixels.
func (c *controller) androidGesture(ctx context.Context, points []Point, ms int) error {
	w, h := c.screen(ctx)
	px := func(p Point) (int, int, error) {
		x, y := int(math.Round(p.X)), int(math.Round(p.Y))
		if x < 0 || y < 0 || (w > 0 && x >= w) || (h > 0 && y >= h) {
			return 0, 0, errInvalid.Errorf("%d, %d is off the %d×%d screen", x, y, w, h)
		}
		return x, y, nil
	}
	x1, y1, err := px(points[0])
	if err != nil {
		return err
	}
	command := fmt.Sprintf("input tap %d %d", x1, y1)
	if len(points) > 1 {
		x2, y2, err := px(points[len(points)-1])
		if err != nil {
			return err
		}
		command = fmt.Sprintf("input swipe %d %d %d %d %d", x1, y1, x2, y2, ms)
	}
	defer c.screenChanged()
	_, err = c.shell(ctx, command)
	return err
}
