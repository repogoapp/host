// Package clilogin runs a CLI's browser sign-in on a machine nobody is sitting
// at: it starts the CLI, reads the link (and code) it prints for a phone to
// open, hands back a code the user pastes when the CLI asks for one, and
// reports the end. gh, Codex and Claude Code sign in through it.
package clilogin

import (
	"bufio"
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/repogo/host/internal/errkind"
)

const (
	// A CLI prints its link before it starts waiting; none by then is a CLI
	// that is not going to print one.
	codeTimeout = 20 * time.Second

	// A pasted code is one token exchange; longer is a CLI that is stuck.
	completeTimeout = time.Minute
)

// CancelGrace outlasts a device-code CLI's 5-second poll: a cancel sent as the
// user comes back from approving must not kill the CLI about to collect the token.
const CancelGrace = 6 * time.Second

var (
	ErrNoCode     = errkind.New(errkind.Unavailable, "sign-in: the CLI did not print a sign-in link")
	ErrNotWaiting = errkind.New(errkind.Invalid, "sign-in: no sign-in is waiting")
	ErrNoPaste    = errkind.New(errkind.Invalid, "sign-in: this sign-in finishes on the web page, not with a pasted code")
	ErrPortInUse  = errkind.New(errkind.Unavailable, "sign-in: the CLI's callback port is in use on this environment")
)

// ansi is the colour a CLI prints even into a pipe.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// Styles of sign-in, which the phone draws differently. A CLI's spec names
// its style; Read only finds the page and any code.
const (
	// StyleDevice shows a code to type on the page; the CLI finishes on its own.
	StyleDevice = "device"
	// StylePaste ends with Complete: the page shows a code and the CLI waits
	// for it on its input.
	StylePaste = "paste"
	// StyleCallback is a page that redirects to a server the CLI runs on this
	// machine's localhost; the phone catches the redirect and forwards it here
	// with forward.fetch.
	StyleCallback = "callback"
)

// Code is what the phone shows: a page to open, how the sign-in ends, and
// for a device code, what to type there.
type Code struct {
	URL   string `json:"url"`
	Style string `json:"style"`

	// UserCode is typed at URL, for StyleDevice.
	UserCode string `json:"user_code,omitempty"`

	ExpiresAtMS int64 `json:"expires_at_ms"`
}

// Spec is one CLI's sign-in.
type Spec struct {
	Command string
	Args    []string
	Env     []string

	// How long the CLI may wait on the user before it is killed.
	Timeout time.Duration

	// Style is how this CLI's sign-in ends, one of the Style constants;
	// empty is StyleDevice.
	Style string

	// TTY runs the CLI on a pseudo-terminal, for one that refuses to sign in
	// without one (flyctl).
	TTY bool

	// Port is the localhost port a StyleCallback CLI listens on. It is checked
	// free first, so a CLI left over from an earlier sign-in is named rather
	// than this one failing as it starts.
	Port int

	// Read is handed each output line, colour stripped, and reports when the
	// code is complete.
	Read func(line string, c *Code) bool

	// OnEnd runs once the CLI exits, after the sign-in stops reading as
	// pending; ok is a clean exit.
	OnEnd func(ok bool)
}

// Logins holds at most one waiting sign-in per key. Log is required.
type Logins struct {
	Log *slog.Logger
	// Grace is how long a cancel waits before stopping the CLI; zero is CancelGrace.
	Grace time.Duration

	mu      sync.Mutex
	pending map[string]*login
}

type login struct {
	// ready closes once the CLI printed its code or failed to; until then the
	// sign-in is starting, not pending, and code, stdin and stop are unset.
	ready    chan struct{}
	started  bool
	startErr error

	code  Code
	stdin io.WriteCloser
	stop  context.CancelFunc

	// cancelling ends a cancelled sign-in after the grace; while it runs
	// the sign-in is not reported as pending.
	cancelling *time.Timer

	// done closes when the CLI exits; err and last are set before.
	done chan struct{}
	err  error
	last string
}

// Start runs the CLI and returns once it has printed its code. A second call
// while one is starting, waiting, or cancelling returns the same code. The
// lock is not held while the CLI starts, so Pending answers meanwhile.
func (l *Logins) Start(key string, s Spec) (Code, error) {
	l.mu.Lock()
	if lg := l.pending[key]; lg != nil {
		if lg.cancelling != nil {
			lg.cancelling.Stop()
			lg.cancelling = nil
		}
		l.mu.Unlock()
		<-lg.ready
		return lg.code, lg.startErr
	}
	if err := portFree(s.Port); err != nil {
		l.mu.Unlock()
		return Code{}, err
	}
	lg := &login{ready: make(chan struct{}), done: make(chan struct{})}
	if l.pending == nil {
		l.pending = map[string]*login{}
	}
	l.pending[key] = lg
	l.mu.Unlock()

	cmd, out, last, err := launch(lg, s, l.Log)
	l.mu.Lock()
	defer l.mu.Unlock()
	defer close(lg.ready)
	if err != nil {
		delete(l.pending, key)
		lg.startErr = err
		return Code{}, err
	}
	lg.started = true
	l.Log.Info("sign-in: waiting on the user", "key", key, "pid", cmd.Process.Pid)
	go l.finish(key, lg, cmd, out, last, s.OnEnd)
	return lg.code, nil
}

// launch starts the CLI and waits, up to codeTimeout, for it to print its code
// into lg; on failure the CLI is stopped.
func launch(lg *login, s Spec, log *slog.Logger) (*exec.Cmd, *os.File, <-chan string, error) {
	ctx, stop := context.WithTimeout(context.Background(), s.Timeout)
	cmd := exec.CommandContext(ctx, s.Command, s.Args...)
	cmd.Env = append(cmd.Environ(), s.Env...)
	out, stdin, err := start(cmd, s.TTY)
	if err != nil {
		stop()
		return nil, nil, nil, err
	}

	found := make(chan Code, 1)
	last := make(chan string, 1)
	go read(out, s, time.Now().Add(s.Timeout), found, last)

	var code Code
	select {
	case code = <-found:
	case <-time.After(codeTimeout):
	}
	if code.URL == "" {
		stop()
		_ = cmd.Wait()
		out.Close()
		line := ""
		select {
		case line = <-last:
		case <-time.After(time.Second):
		}
		log.Warn("sign-in: no link printed", "cmd", s.Command, "out", line)
		return nil, nil, nil, ErrNoCode
	}
	lg.code, lg.stdin, lg.stop = code, stdin, stop
	return cmd, out, last, nil
}

// start runs cmd with one stream for its output, since gh prints to stderr
// and Codex and Claude to stdout, and one for its input. On a pseudo-terminal
// both are the terminal.
func start(cmd *exec.Cmd, tty bool) (*os.File, io.WriteCloser, error) {
	// Stopping the sign-in stops its whole process group: wrangler's launcher
	// runs the CLI as a child that would otherwise keep its callback port.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	if tty {
		// pty starts it in a new session, which is its own process group.
		terminal, err := pty.Start(cmd)
		return terminal, terminal, err
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, err
	}
	out, w, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdout, cmd.Stderr = w, w
	err = cmd.Start()
	w.Close()
	if err != nil {
		out.Close()
		return nil, nil, err
	}
	return out, stdin, nil
}

// read scans the CLI's output to the end: up to the code for Start, then for
// the last line, which says why it exited. Drained, because a CLI blocks on a
// full pipe.
func read(out io.Reader, s Spec, expires time.Time, found chan<- Code, last chan<- string) {
	code := Code{Style: cmp.Or(s.Style, StyleDevice), ExpiresAtMS: expires.UnixMilli()}
	sent, line := false, ""
	scanner := bufio.NewScanner(out)
	for scanner.Scan() {
		t := strings.TrimSpace(ansi.ReplaceAllString(scanner.Text(), ""))
		if t == "" {
			continue
		}
		line = t
		if !sent && s.Read(t, &code) {
			found <- code
			sent = true
		}
	}
	if !sent {
		found <- Code{}
	}
	last <- line
}

func (l *Logins) finish(key string, lg *login, cmd *exec.Cmd, out *os.File, last <-chan string, onEnd func(bool)) {
	err := cmd.Wait()
	lg.stop()
	// A child the CLI left behind can hold the pipe open; the exit is the end.
	var line string
	select {
	case line = <-last:
	case <-time.After(time.Second):
		out.Close()
		select {
		case line = <-last:
		case <-time.After(time.Second):
		}
	}
	out.Close()

	l.mu.Lock()
	if l.pending[key] == lg {
		delete(l.pending, key)
	}
	l.mu.Unlock()
	lg.err, lg.last = err, line
	close(lg.done)

	if err != nil {
		l.Log.Warn("sign-in: ended without signing in", "key", key, "err", err, "out", line)
	} else {
		l.Log.Info("sign-in: signed in", "key", key, "out", line)
	}
	if onEnd != nil {
		onEnd(err == nil)
	}
}

// Complete hands the CLI the code the user pasted and waits for it to exit.
// The CLI's own last line is the error when the code is refused.
func (l *Logins) Complete(ctx context.Context, key, code string) error {
	l.mu.Lock()
	lg := l.pending[key]
	started := lg != nil && lg.started
	l.mu.Unlock()
	if !started {
		return ErrNotWaiting
	}
	if lg.code.Style != StylePaste {
		return ErrNoPaste
	}
	code = strings.TrimSpace(code)
	if code == "" || strings.ContainsAny(code, "\r\n") {
		return fmt.Errorf("%w: the code is empty", errkind.ErrInvalid)
	}
	if _, err := io.WriteString(lg.stdin, code+"\n"); err != nil {
		return err
	}
	select {
	case <-lg.done:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(completeTimeout):
		return fmt.Errorf("sign-in: %s is still working on the code", key)
	}
	if lg.err != nil {
		if lg.last != "" {
			return fmt.Errorf("sign-in: %s", lg.last)
		}
		return fmt.Errorf("sign-in: %w", lg.err)
	}
	return nil
}

// Cancel stops a waiting sign-in after the grace; one approved in the
// meantime still lands. Nothing waiting is not an error.
func (l *Logins) Cancel(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lg := l.pending[key]; lg != nil && lg.started && lg.cancelling == nil {
		grace := cmp.Or(l.Grace, CancelGrace)
		l.Log.Info("sign-in: cancelled by a device", "key", key, "grace", grace)
		lg.cancelling = time.AfterFunc(grace, lg.stop)
	}
}

// Close stops every sign-in at once, with no grace, and waits for each CLI
// to exit: the host is shutting down, and a CLI it left would keep its port.
func (l *Logins) Close() {
	l.mu.Lock()
	running := make([]*login, 0, len(l.pending))
	for _, lg := range l.pending {
		if lg.started {
			lg.stop()
			running = append(running, lg)
		}
	}
	l.mu.Unlock()
	for _, lg := range running {
		select {
		case <-lg.done:
		case <-time.After(5 * time.Second):
			l.Log.Warn("sign-in: CLI did not exit at shutdown")
		}
	}
}

// portFree reports whether a callback port can be listened on; zero is no port.
func portFree(port int) error {
	if port == 0 {
		return nil
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return fmt.Errorf("%w (port %d); an earlier sign-in may still be running", ErrPortInUse, port)
	}
	return ln.Close()
}

// Pending is the waiting sign-in's code, nil when none waits.
func (l *Logins) Pending(key string) *Code {
	l.mu.Lock()
	defer l.mu.Unlock()
	lg := l.pending[key]
	if lg == nil || !lg.started || lg.cancelling != nil {
		return nil
	}
	code := lg.code
	return &code
}
