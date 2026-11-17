package screens

import (
	"context"
	"encoding/json"
	"net"
	"os/user"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/screens/vnc"
)

// DisplayID is this machine's own display among the screens.
const DisplayID = "display"

// screenSharing is where Apple's Screen Sharing serves VNC on this Mac.
const screenSharing = "127.0.0.1:5900"

// ThisDisplay is the Mac's own screen as a device, while Screen Sharing listens
// on loopback for the host to read it through.
func ThisDisplay(ctx context.Context) (Screen, bool) {
	if runtime.GOOS != "darwin" {
		return Screen{}, false
	}
	conn, err := (&net.Dialer{Timeout: 300 * time.Millisecond}).DialContext(ctx, "tcp", screenSharing)
	if err != nil {
		return Screen{}, false
	}
	conn.Close()
	name, _ := run(ctx, "scutil", "--get", "ComputerName")
	version, _ := run(ctx, "sw_vers", "-productVersion")
	account, _ := user.Current()
	d := Screen{
		Kind: KindDisplay, Platform: PlatformMacOS, ID: DisplayID, Name: strings.TrimSpace(string(name)),
		OS: OS{Name: "macOS", Version: strings.TrimSpace(string(version))}, State: StateBooted, Form: FormDesktop,
	}
	if account != nil {
		d.User = account.Username
	}
	return d, true
}

// streamDisplay runs `repogo screencap`, which reads this Mac's screen from
// Screen Sharing signed in with password, handed to it on its first input
// line; nothing on the Mac keeps it. ready goes out in front of the first
// frame, which comes only once Screen Sharing took the password: a refusal
// is the stream's failure with no ready before it.
func streamDisplay(ctx context.Context, c *controller, password string, ready func() error,
	send func([]byte) error, input *inputQueue) error {
	if password == "" {
		return newError(errkind.Denied, ErrorAuthRequired, "This display needs your login password.")
	}
	refusedMu.Lock()
	wait := refusalWait - time.Since(refusedAt)
	refusedMu.Unlock()
	if wait > 0 {
		return newError(errkind.Denied, ErrorAuthRejected, "Screen Sharing refused the password. Wait a moment and try again.")
	}
	first, err := json.Marshal(map[string]string{"password": password})
	if err != nil {
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
		return send(b)
	}
	err = streamChild(ctx, c, frame, input, []string{"screencap"}, first)
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
