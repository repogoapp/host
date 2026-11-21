package screens

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/screens/desktop"
)

// A Windows PC streams its own display, read with Desktop Duplication in
// the session the host runs in, as the user: no password.
const displayNeedsPassword = false

// hasOwnDisplay: the Windows screen is early, so it's offered only by a
// host started with REPOGO_WINDOWS_SCREEN=1 (the test VMs). Without it a
// Windows host lists no display of its own.
var hasOwnDisplay = os.Getenv("REPOGO_WINDOWS_SCREEN") == "1"

// thisDisplay is this PC's screen, while the host runs in a signed-in
// session: one started as a service (session 0) has no desktop to read.
func thisDisplay(context.Context) (Screen, bool) {
	if !hasOwnDisplay {
		return Screen{}, false
	}
	if session, ok := ownSession(); !ok || session == 0 {
		return Screen{}, false
	}
	name, _ := os.Hostname()
	d := Screen{
		Kind: KindDisplay, Platform: PlatformWindows, ID: DisplayID, Name: name,
		OS: windowsVersion(), State: StateBooted, Form: FormDesktop,
	}
	if account, err := user.Current(); err == nil {
		// DOMAIN\name: the name alone, as a Mac's account shows.
		d.User = account.Username[strings.LastIndex(account.Username, `\`)+1:]
	}
	return d, true
}

// windowsVersion is "Windows" and its release (24H2), or its build when it
// names no release.
func windowsVersion() OS {
	info := OS{Name: "Windows"}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return info
	}
	defer k.Close()
	if v, _, err := k.GetStringValue("DisplayVersion"); err == nil && v != "" {
		info.Version = v
	} else if v, _, err := k.GetStringValue("CurrentBuild"); err == nil {
		info.Version = v
	}
	return info
}

// ownSession is the Windows session the host runs in.
func ownSession() (uint32, bool) {
	var session uint32
	return session, windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &session) == nil
}

// The phone stops retrying on ErrorNotRunning and shows the message with
// Retry: nothing changes until someone signs in at the PC.
var (
	errDisconnected = newError(errkind.Unavailable, ErrorNotRunning,
		"Windows has no screen to show: its session was left disconnected (Remote Desktop closed). "+
			"Sign in at the PC, then tap Retry.")
	errLocked = newError(errkind.Unavailable, ErrorNotRunning,
		"Windows is locked. Sign in at the PC, then tap Retry.")
)

// readyDisplay gets the desktop showing before a stream starts. A session
// left disconnected (Remote Desktop closed) is moved back to the PC's own
// screen when the host runs with admin rights; without them, or while
// Windows is locked, the stream is refused with what to do.
func readyDisplay(ctx context.Context) error {
	session, ok := ownSession()
	if !ok {
		return nil
	}
	if sessionState(session) == windows.WTSDisconnected {
		if !windows.GetCurrentProcessToken().IsElevated() || reattach(ctx, session) != nil {
			return errDisconnected
		}
	}
	if desktop.Locked() {
		return errLocked
	}
	return nil
}

// sessionState is a session's connect state (WTSActive, WTSDisconnected…),
// -1 when Windows doesn't list it.
func sessionState(session uint32) int {
	var list *windows.WTS_SESSION_INFO
	var count uint32
	if windows.WTSEnumerateSessions(0, 0, 1, &list, &count) != nil {
		return -1
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(list)))
	for _, s := range unsafe.Slice(list, count) {
		if s.SessionID == session {
			return int(s.State)
		}
	}
	return -1
}

// reattachTask is the one-time task that moves a session to the screen.
const reattachTask = `RepoGo\Reattach`

// reattach moves a disconnected session to the PC's own screen, as
// `tscon <session> /dest:console`. Only SYSTEM may do that without the
// account's password, so it runs as a one-time SYSTEM task, which takes
// admin rights to make. It waits for the session to be active.
func reattach(ctx context.Context, session uint32) error {
	system32 := filepath.Join(os.Getenv("SystemRoot"), "System32")
	run := func(args ...string) error {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, filepath.Join(system32, "schtasks.exe"), args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("schtasks %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	tscon := fmt.Sprintf(`"%s" %d /dest:console`, filepath.Join(system32, "tscon.exe"), session)
	if err := run("/Create", "/TN", reattachTask, "/TR", tscon, "/SC", "ONCE", "/ST", "00:00",
		"/RU", "SYSTEM", "/RL", "HIGHEST", "/F"); err != nil {
		return err
	}
	defer run("/Delete", "/TN", reattachTask, "/F")
	if err := run("/Run", "/TN", reattachTask); err != nil {
		return err
	}
	for range 30 {
		if sessionState(session) == windows.WTSActive {
			// The desktop draws a moment after the session comes back.
			time.Sleep(time.Second)
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("session %d didn't come back to the screen", session)
}
