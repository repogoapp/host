package screens

import (
	"context"
	"net"
	"os/user"
	"strings"
	"time"
)

// A Mac streams its own display, read from Screen Sharing with the
// account's password.
const (
	hasOwnDisplay        = true
	displayNeedsPassword = true
)

// screenSharing is where Apple's Screen Sharing serves VNC on this Mac.
const screenSharing = "127.0.0.1:5900"

// thisDisplay is the Mac's own screen, while Screen Sharing listens on
// loopback for the host to read it through.
func thisDisplay(ctx context.Context) (Screen, bool) {
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

// readyDisplay has nothing to get ready here: Screen Sharing shows what
// the Mac shows, its lock screen included.
func readyDisplay(context.Context) error { return nil }
