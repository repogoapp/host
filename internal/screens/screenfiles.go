package screens

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/repogoapp/host/internal/apphome"
)

// Screenshots and recordings saved for agents and `repogo screens`: each a
// new file under the host's tmp/screens, swept after a day.

// SaveScreenshot writes the screen as a PNG under the host's folder, a new
// file each time, and answers its path.
func (m *Manager) SaveScreenshot(ctx context.Context, id string) (string, error) {
	c, err := m.agentController(ctx, id)
	if err != nil {
		return "", err
	}
	var shot []byte
	if c.d.Platform == PlatformIOS {
		if err := c.knowPanel(ctx); err != nil {
			return "", err
		}
		shot, err = c.screenshotIOSAs(ctx, "png")
	} else {
		shot, err = c.screencapAndroid(ctx)
	}
	if err != nil {
		return "", err
	}
	if shot, err = c.maskSecure(ctx, shot); err != nil {
		return "", err
	}
	path, err := newScreenFile(id, ".png")
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(path, shot, 0o600)
}

// maskSecure paints the screen's password fields black, from the last read
// or a new one, so a screenshot never shows one; one it can't place on the
// picture fails rather than show it.
func (c *controller) maskSecure(ctx context.Context, shot []byte) ([]byte, error) {
	read := c.lastRead()
	if read == nil {
		c.agent.lock.Lock()
		r, err := c.readElements(ctx)
		c.agent.lock.Unlock()
		if err != nil {
			return nil, err
		}
		read = &r
	}
	var secure []Element
	for _, e := range read.Elements {
		if e.Secure {
			secure = append(secure, e)
		}
	}
	if len(secure) == 0 {
		return shot, nil
	}
	img, _, err := image.Decode(bytes.NewReader(shot))
	if err != nil {
		return nil, err
	}
	size := img.Bounds().Size()
	scale := float64(size.X) / read.Width
	if read.Width < 1 || math.Abs(float64(size.Y)/read.Height-scale) > 0.05*scale {
		return nil, errors.New("a password field is on the screen and the screenshot couldn't be masked: try again")
	}
	out := image.NewRGBA(img.Bounds())
	draw.Draw(out, out.Bounds(), img, img.Bounds().Min, draw.Src)
	for _, e := range secure {
		r := image.Rect(int(e.X*scale), int(e.Y*scale), int(math.Ceil((e.X+e.W)*scale)), int(math.Ceil((e.Y+e.H)*scale)))
		draw.Draw(out, r.Add(img.Bounds().Min), image.Black, image.Point{}, draw.Src)
	}
	var b bytes.Buffer
	if err := png.Encode(&b, out); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// agentImageBytes keeps a screenshot an agent looks at small: a few hundred
// KB of JPEG reads the screen as well as the full PNG, for far fewer tokens.
const agentImageBytes = 300 << 10

// ScreenshotForAgent is a saved screenshot's picture as an agent takes it in
// its tool answer: a JPEG no bigger than agentImageBytes.
func ScreenshotForAgent(path string) ([]byte, error) {
	shot, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return asJPEG(shot, agentImageBytes)
}

// knowPanel makes sure the host knows which panel a foldable shows, which
// simctl's screenshots and recordings need: a view reports it, so one is
// started for agents when none is open.
func (c *controller) knowPanel(ctx context.Context) error {
	c.mu.Lock()
	known := c.activeScreen != 0 || !c.universalHID()
	c.mu.Unlock()
	if known {
		return nil
	}
	_, err := c.inputPath(ctx)
	return err
}

// screenFileAge is how long a screenshot or recording stays: long enough to
// share or look at, then each new one sweeps the old.
const screenFileAge = 24 * time.Hour

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// newScreenFile makes a new, empty file under the host's folder for
// screens, never an existing one, and answers its path.
func newScreenFile(id, ext string) (string, error) {
	name := unsafeName.ReplaceAllString(id, "-") + "-" + strconv.FormatInt(time.Now().UnixMilli(), 10)
	dir, err := apphome.Path("tmp", "screens")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	sweepScreenFiles(dir, time.Now().Add(-screenFileAge))
	for i := 0; i < 10; i++ {
		path := filepath.Join(dir, name+ext)
		if i > 0 {
			path = filepath.Join(dir, fmt.Sprintf("%s-%d%s", name, i, ext))
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		return path, f.Close()
	}
	return "", errors.New("couldn't name the file")
}

// sweepScreenFiles removes the files in dir last changed before cutoff.
func sweepScreenFiles(dir string, cutoff time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() && info.ModTime().Before(cutoff) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
