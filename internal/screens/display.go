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

	"github.com/repogoapp/host/internal/screens/capture"
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
		Kind: KindDisplay, Platform: "macos", ID: DisplayID, Name: strings.TrimSpace(string(name)),
		OS: "macOS " + strings.TrimSpace(string(version)), Booted: true,
	}
	if account != nil {
		d.User = account.Username
	}
	return d, true
}

// streamDisplay runs `repogo screencap`, which reads this Mac's screen from
// Screen Sharing signed in with password, handed to it on its first input
// line; nothing on the Mac keeps it.
func streamDisplay(ctx context.Context, c *controller, password string, send func([]byte) error,
	input <-chan capture.Input) error {
	if password == "" {
		return &StreamError{Code: "auth_required", Message: "This display needs your login password."}
	}
	refusedMu.Lock()
	wait := refusalWait - time.Since(refusedAt)
	refusedMu.Unlock()
	if wait > 0 {
		return &StreamError{Code: "auth_rejected", Message: "Screen Sharing refused the password. Wait a moment and try again."}
	}
	first, err := json.Marshal(map[string]string{"password": password})
	if err != nil {
		return err
	}
	err = streamChild(ctx, c, send, input, []string{"screencap"}, first)
	if err != nil && err.Error() == vnc.ErrRefused.Error() {
		refusedMu.Lock()
		refusedAt = time.Now()
		refusedMu.Unlock()
		return &StreamError{Code: "auth_rejected", Message: vnc.ErrRefused.Error()}
	}
	return err
}

// StreamError is a live view's failure the phone acts on by its Code:
// auth_required and auth_rejected ask it for the display's login password.
type StreamError struct {
	Code    string
	Message string
}

func (e *StreamError) Error() string { return e.Message }

func (e *StreamError) StreamCode() string { return e.Code }

// refusedAt is when Screen Sharing last refused a password: another try
// waits refusalWait, so a stream of guesses can't run at full speed.
var (
	refusedMu sync.Mutex
	refusedAt time.Time
)

const refusalWait = 3 * time.Second
