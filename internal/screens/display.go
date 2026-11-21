package screens

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/screens/vnc"
)

// DisplayID is this machine's own display among the screens.
const DisplayID = "display"

// ThisDisplay is this machine's own screen as a device, when it can be
// streamed: a Mac's while Screen Sharing listens on loopback, a Windows
// PC's while the host runs in a signed-in session (display_<os>.go).
func ThisDisplay(ctx context.Context) (Screen, bool) { return thisDisplay(ctx) }

// streamDisplay runs `repogo screencap`, which reads this machine's screen.
// On a Mac it signs in to Screen Sharing with password, sent on its first
// input line and kept nowhere. ready goes out before the first frame, once
// Screen Sharing took the password, so a refusal fails with no ready first.
// Windows needs no password: the host already runs as the user.
func streamDisplay(ctx context.Context, c *controller, password string, ready func() error,
	framed *framer, input *inputQueue) error {
	var first []byte
	if displayNeedsPassword {
		if password == "" {
			return newError(errkind.Denied, ErrorAuthRequired, "This display needs your login password.")
		}
		refusedMu.Lock()
		wait := refusalWait - time.Since(refusedAt)
		refusedMu.Unlock()
		if wait > 0 {
			return newError(errkind.Denied, ErrorAuthRejected, "Screen Sharing refused the password. Wait a moment and try again.")
		}
		var err error
		if first, err = json.Marshal(map[string]string{"password": password}); err != nil {
			return err
		}
	}
	if err := readyDisplay(ctx); err != nil {
		return err
	}
	watch, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	go c.watchDesktop(watch)
	accepted := false
	frame := func(b []byte) error {
		if !accepted {
			accepted = true
			if err := ready(); err != nil {
				return err
			}
		}
		return framed.child(b)
	}
	err := streamChild(ctx, c, frame, framed.pace, input, []string{"screencap"}, first)
	if err != nil && err.Error() == vnc.ErrRefused.Error() {
		refusedMu.Lock()
		refusedAt = time.Now()
		refusedMu.Unlock()
		return newError(errkind.Denied, ErrorAuthRejected, vnc.ErrRefused.Error())
	}
	return err
}

// refusedAt is when Screen Sharing last refused a password: another try
// waits refusalWait, so a stream of guesses can't run at full speed.
var (
	refusedMu sync.Mutex
	refusedAt time.Time
)

const refusalWait = 3 * time.Second
