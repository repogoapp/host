package screens

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	_ "image/png" // Android's screencap writes PNG
	"net/url"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/screens/capture"
)

// Settings are the settings screens.set changes: only those given.
type Settings struct {
	Appearance       *Appearance  `json:"appearance,omitempty"`
	ContentSize      *ContentSize `json:"content_size,omitempty"`
	Contrast         *bool        `json:"contrast,omitempty"`
	StatusBar        *bool        `json:"status_bar,omitempty"`
	SlowAnimations   *bool        `json:"slow_animations,omitempty"`
	OnscreenKeyboard *bool        `json:"onscreen_keyboard,omitempty"`
	Pose             *Pose        `json:"pose,omitempty"`
}

// Coordinate is a simulated location.
type Coordinate struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

var errInvalid = errkind.ErrInvalid

// Set changes the settings given, in order, and answers the state after.
func (m *Manager) Set(ctx context.Context, id string, s Settings) (State, error) {
	c, err := m.controller(ctx, id)
	if err != nil {
		return State{}, err
	}
	// Whether a simulator folds is known once a view has reported its panels:
	// a fold asked for before any (from the CLI or an agent) starts one first.
	if s.Pose != nil && c.d.Platform == PlatformIOS {
		if err := c.knowPanel(ctx); err != nil {
			return State{}, err
		}
	}
	checks := []struct {
		given   bool
		control Control
		valid   bool
	}{
		{s.Appearance != nil, ControlAppearance, s.Appearance == nil || *s.Appearance == AppearanceLight || *s.Appearance == AppearanceDark},
		{s.ContentSize != nil, ControlContentSize, s.ContentSize == nil || s.ContentSize.valid()},
		{s.Contrast != nil, ControlContrast, true},
		{s.StatusBar != nil, ControlStatusBar, true},
		{s.SlowAnimations != nil, ControlSlowAnimations, true},
		{s.OnscreenKeyboard != nil, ControlOnscreenKeyboard, true},
		{s.Pose != nil, ControlPose, s.Pose == nil || validPose(*s.Pose)},
	}
	for _, check := range checks {
		if !check.given {
			continue
		}
		if !check.valid {
			return State{}, errInvalid.Errorf("unknown %s", check.control)
		}
		if err := c.supports(check.control); err != nil {
			return State{}, err
		}
	}
	if s.Pose != nil {
		defer c.screenChanged()
	}
	if c.d.Platform == PlatformIOS {
		err = c.setIOS(ctx, s)
	} else {
		err = c.setAndroid(ctx, s)
	}
	c.mu.Lock()
	state := c.state
	c.mu.Unlock()
	return state, err
}

func validPose(p Pose) bool {
	return p == PoseClosed || p == PoseOpen || p == PoseBook || p == PoseLaptop
}

// Rotate turns the device a quarter left or right.
func (m *Manager) Rotate(ctx context.Context, id string, t Turn) error {
	if t != TurnLeft && t != TurnRight {
		return errInvalid.Errorf("unknown turn %q", t)
	}
	c, err := m.act(ctx, id, ControlRotate)
	if err != nil {
		return err
	}
	defer c.screenChanged()
	if c.d.Platform == PlatformIOS {
		return c.rotateIOS(ctx, t)
	}
	return c.rotateAndroid(ctx, t)
}

// OpenURL opens a link on the device, in whichever app takes it.
func (m *Manager) OpenURL(ctx context.Context, id, link string) error {
	if u, err := url.Parse(link); err != nil || u.Scheme == "" {
		return errInvalid.Errorf("not a link: %q", link)
	}
	c, err := m.act(ctx, id, ControlOpenURL)
	if err != nil {
		return err
	}
	defer c.screenChanged()
	if c.d.Platform == PlatformIOS {
		_, err = c.simctl(ctx, "openurl", c.d.ID, link)
		return err
	}
	_, err = c.shell(ctx, "am start -a android.intent.action.VIEW -d "+shellQuote(link))
	return err
}

// Location sets the device's location; nil clears it.
func (m *Manager) Location(ctx context.Context, id string, at *Coordinate) error {
	c, err := m.act(ctx, id, ControlLocation)
	if err != nil {
		return err
	}
	if c.d.Platform == PlatformIOS {
		return c.locationIOS(ctx, at)
	}
	return c.locationAndroid(ctx, at)
}

// Push delivers a push notification payload to an app on an iOS simulator;
// an empty bundle reads the app from the payload.
func (m *Manager) Push(ctx context.Context, id string, payload []byte, bundle string) error {
	c, err := m.act(ctx, id, ControlPush)
	if err != nil {
		return err
	}
	return c.pushIOS(ctx, payload, bundle)
}

// Shake shakes an iOS simulator.
func (m *Manager) Shake(ctx context.Context, id string) error {
	c, err := m.act(ctx, id, ControlShake)
	if err != nil {
		return err
	}
	_, err = c.simctl(ctx, "spawn", c.d.ID, "notifyutil", "-p", "com.apple.UIKit.SimulatorShake")
	return err
}

// MemoryWarning sends an iOS simulator's apps a memory warning, through its live view.
func (m *Manager) MemoryWarning(ctx context.Context, id string) error {
	c, err := m.act(ctx, id, ControlMemoryWarning)
	if err != nil {
		return err
	}
	return c.command(ctx, capture.Input{Action: capture.ActionMemoryWarning})
}

// maxScreenshot keeps a screenshot well inside a reply's 8 MB.
const maxScreenshot = 5 << 20

// Screenshot is the screen as a JPEG.
func (m *Manager) Screenshot(ctx context.Context, id string) ([]byte, error) {
	c, err := m.act(ctx, id, ControlScreenshot)
	if err != nil {
		return nil, err
	}
	var shot []byte
	if c.d.Platform == PlatformIOS {
		shot, err = c.screenshotIOS(ctx)
	} else {
		shot, err = c.screencapAndroid(ctx)
	}
	if err != nil {
		return nil, err
	}
	return asJPEG(shot, maxScreenshot)
}

// asJPEG re-encodes a PNG, and a JPEG past limit, at a quality that fits;
// one still too big at the lowest is halved until it does.
func asJPEG(shot []byte, limit int) ([]byte, error) {
	if len(shot) > 2 && shot[0] == 0xff && shot[1] == 0xd8 && len(shot) <= limit {
		return shot, nil
	}
	img, _, err := image.Decode(bytes.NewReader(shot))
	if err != nil {
		return nil, err
	}
	for {
		var out bytes.Buffer
		for _, quality := range []int{85, 70, 50} {
			out.Reset()
			if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: quality}); err != nil {
				return nil, err
			}
			if out.Len() <= limit {
				return out.Bytes(), nil
			}
		}
		if size := img.Bounds().Size(); size.X < 2 || size.Y < 2 {
			return out.Bytes(), nil
		}
		img = halve(img)
	}
}

// halve is img at half its width and height, each pixel the mean of four.
func halve(img image.Image) image.Image {
	from := img.Bounds().Min
	out := image.NewRGBA(image.Rect(0, 0, img.Bounds().Dx()/2, img.Bounds().Dy()/2))
	for y := range out.Rect.Dy() {
		for x := range out.Rect.Dx() {
			var sum [4]uint32
			for _, p := range [4]image.Point{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
				r, g, b, a := img.At(from.X+2*x+p.X, from.Y+2*y+p.Y).RGBA()
				sum[0], sum[1], sum[2], sum[3] = sum[0]+r, sum[1]+g, sum[2]+b, sum[3]+a
			}
			at := out.PixOffset(x, y)
			for i, v := range sum {
				out.Pix[at+i] = uint8(v >> 10) // the mean of four 16-bit values, as 8 bits
			}
		}
	}
	return out
}

// act is the screen's controller when it takes control.
func (m *Manager) act(ctx context.Context, id string, control Control) (*controller, error) {
	c, err := m.controller(ctx, id)
	if err != nil {
		return nil, err
	}
	return c, c.supports(control)
}
