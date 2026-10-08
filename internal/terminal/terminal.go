// Package terminal is PTY-backed shell sessions that outlive the connection
// that created them and are shared by every paired device; only the watcher
// set is per device. Bytes are opaque: xterm.js in the client is the emulator.
package terminal

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/google/uuid"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/files"
)

const (
	// scrollback is what a reattaching client is shown. Matches the webview's
	// own buffer cap, so the two ends agree on how much history exists.
	scrollbackMax = 100 * 1024

	readBuffer = 32 * 1024

	// flushInterval batches PTY reads into one push. A build's output arrives
	// as thousands of tiny writes, and one relay frame each would spend more
	// time framing than the shell spends producing.
	flushInterval = 16 * time.Millisecond

	// maxSessions is per host, across all devices. Each session is a shell, a
	// PTY and two goroutines; unbounded, a client that creates one per screen
	// leaks them all.
	maxSessions = 16

	maxDim = 500

	// watchLease is how long a client keeps being told about a project's tabs
	// after its last renewal; the host cannot see a disconnect through the relay.
	watchLease = 2 * time.Minute
)

var (
	ErrNotFound  = errkind.New(errkind.NotFound, "terminal: no such session")
	ErrTooMany   = errkind.New(errkind.Unavailable, "terminal: too many open sessions")
	ErrNoProject = errkind.New(errkind.Invalid, "terminal: a project directory is required")
)

// Info is one session on the wire.
type Info struct {
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Shell     string `json:"shell"`
	PID       int    `json:"pid"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
	CreatedAt int64  `json:"created_at_ms"`
}

type Manager struct {
	paths files.Container
	log   *slog.Logger

	// emit is how a session reports output and exit to its watchers.
	emit *emit.Emitter

	mu       sync.Mutex
	sessions map[string]*session

	// watch is who wants a project's tab set, keyed by project directory. Separate
	// from session watchers because a client shows the tab strip before attaching.
	watch map[string]map[device.ID]time.Time
}

type session struct {
	id        string
	cwd       string
	shell     string
	cols      int
	rows      int
	createdAt time.Time

	pty *os.File
	cmd *exec.Cmd

	mu         sync.Mutex
	scrollback []byte
	pending    []byte
	watchers   map[device.ID]struct{}
	done       bool
}

func New(paths files.Container, emit *emit.Emitter, log *slog.Logger) *Manager {
	return &Manager{
		paths: paths, emit: emit, log: log,
		sessions: map[string]*session{},
		watch:    map[string]map[device.ID]time.Time{},
	}
}

// LoginShell is the user's own shell, which is what their terminal would run.
// Containers often start the host without SHELL and without zsh, so it falls
// back to the first shell on PATH.
func LoginShell() string {
	if shell := os.Getenv("SHELL"); shell != "" {
		if _, err := os.Stat(shell); err == nil {
			return shell
		}
	}
	for _, name := range []string{"zsh", "bash", "sh"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	return "/bin/sh"
}

// Create starts a shell in a project directory and attaches the caller.
func (m *Manager) Create(caller device.ID, cwd string, cols, rows int) (Info, error) {
	if cwd == "" {
		return Info{}, ErrNoProject
	}
	dir, err := m.paths.Contain(cwd)
	if err != nil {
		return Info{}, err
	}
	cols, rows = clamp(cols, 80), clamp(rows, 24)

	if m.Open() >= maxSessions {
		return Info{}, ErrTooMany
	}

	// A login shell with the user's dotfiles, so their PATH, aliases and
	// installed agent CLIs are all there.
	shell := LoginShell()
	cmd := exec.Command(shell, "-l")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color", "COLORTERM=truecolor")

	s := newSession(dir, shell, cmd, cols, rows)
	s.watchers[caller] = struct{}{}
	if err := m.start(s); err != nil {
		return Info{}, err
	}
	m.log.Info("terminal: session opened", "session", s.id, "cwd", dir, "shell", shell)
	return s.info(), nil
}

func newSession(cwd, shell string, cmd *exec.Cmd, cols, rows int) *session {
	return &session{
		id: uuid.NewString(), cwd: cwd, shell: shell, cols: cols, rows: rows,
		createdAt: time.Now(), cmd: cmd,
		watchers: map[device.ID]struct{}{},
	}
}

// start runs the session's command on a new PTY and shows its tab.
func (m *Manager) start(s *session) error {
	f, err := pty.StartWithSize(s.cmd, &pty.Winsize{Cols: uint16(s.cols), Rows: uint16(s.rows)})
	if err != nil {
		return fmt.Errorf("terminal: start %s: %w", s.shell, err)
	}
	s.pty = f
	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()

	go m.pump(s)
	go m.flush(s)
	m.changed(s.cwd)
	return nil
}

// List is the sessions in one project, or every session when cwd is empty.
// Oldest first, which is the tab order, so every client agrees without storing
// one.
func (m *Manager) List(cwd string) []Info {
	if cwd != "" {
		if dir, err := m.paths.Contain(cwd); err == nil {
			cwd = dir
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.sessions))
	for _, s := range m.sessions {
		if cwd == "" || s.cwd == cwd {
			out = append(out, s.info())
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

// Subscribe asks to be told when a project's tab set changes, and renews the
// lease. Calling it again with the same project is how a client stays
// subscribed — same shape as internal/projectwatch, for the same reason.
func (m *Manager) Subscribe(caller device.ID, cwd string) error {
	dir, err := m.paths.Contain(cwd)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.watch[dir] == nil {
		m.watch[dir] = map[device.ID]time.Time{}
	}
	m.watch[dir][caller] = time.Now().Add(watchLease)
	return nil
}

func (m *Manager) Unsubscribe(caller device.ID, cwd string) {
	dir, err := m.paths.Contain(cwd)
	if err != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.watch[dir], caller)
	if len(m.watch[dir]) == 0 {
		delete(m.watch, dir)
	}
}

// changed pushes a project's whole tab set to its watchers; a snapshot cannot
// drift the way a replayed delta can.
func (m *Manager) changed(cwd string) {
	sessions := m.List(cwd)

	m.mu.Lock()
	now := time.Now()
	watchers := make([]device.ID, 0, len(m.watch[cwd]))
	for id, expiry := range m.watch[cwd] {
		if expiry.Before(now) {
			delete(m.watch[cwd], id)
			continue
		}
		watchers = append(watchers, id)
	}
	m.mu.Unlock()

	ev := Changed{Cwd: cwd, Sessions: sessions}
	for _, id := range watchers {
		_ = m.emit.To(id, ev)
	}
}

// Attach subscribes the caller to a session's output and returns the scrollback
// it missed, snapshotted under the append lock so no chunk is lost or doubled.
func (m *Manager) Attach(caller device.ID, id string) ([]byte, Info, error) {
	s, err := m.lookup(id)
	if err != nil {
		return nil, Info{}, err
	}
	s.mu.Lock()
	s.watchers[caller] = struct{}{}
	// Everything except what has not been flushed yet. The unflushed tail is
	// in the scrollback AND still queued for the next push, so returning it
	// here too would print those bytes twice on the attaching client.
	held := min(len(s.pending), len(s.scrollback))
	snapshot := s.scrollback[:len(s.scrollback)-held]
	buffered := make([]byte, len(snapshot))
	copy(buffered, snapshot)
	s.mu.Unlock()
	return buffered, s.info(), nil
}

// Detach stops pushing to one device without touching the shell. A session
// that already exited is detached; the two race.
func (m *Manager) Detach(caller device.ID, id string) error {
	s, err := m.lookup(id)
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.watchers, caller)
	return nil
}

// Input writes keystrokes to the shell. Raw bytes, including control
// characters: ^C is 0x03 and nothing here needs to know that.
func (m *Manager) Input(id string, data []byte) error {
	s, err := m.lookup(id)
	if err != nil {
		return err
	}
	if _, err := s.pty.Write(data); err != nil {
		return fmt.Errorf("terminal: write: %w", err)
	}
	return nil
}

func (m *Manager) Resize(id string, cols, rows int) error {
	s, err := m.lookup(id)
	if err != nil {
		return err
	}
	cols, rows = clamp(cols, 80), clamp(rows, 24)
	if err := pty.Setsize(s.pty, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)}); err != nil {
		return fmt.Errorf("terminal: resize: %w", err)
	}
	s.mu.Lock()
	s.cols, s.rows = cols, rows
	s.mu.Unlock()
	return nil
}

// Close kills the shell. The pump sees the PTY end and announces the exit, so
// closing and the process dying on its own take the same path; closing one
// that already exited is success, since the two race.
func (m *Manager) Close(id string) error {
	s, err := m.lookup(id)
	if err != nil {
		return nil
	}
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	return s.pty.Close()
}

// Open is how many of the user's shells are running.
func (m *Manager) Open() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Shutdown kills every shell. Called when the host itself is going away, where
// leaving orphaned logins attached to a closed PTY helps nobody.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	all := make([]*session, 0, len(m.sessions))
	for _, s := range m.sessions {
		all = append(all, s)
	}
	m.mu.Unlock()

	for _, s := range all {
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		_ = s.pty.Close()
	}
}

func (m *Manager) lookup(id string) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return s, nil
}

// pump reads the PTY until the shell ends.
func (m *Manager) pump(s *session) {
	buf := make([]byte, readBuffer)
	for {
		n, err := s.pty.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.scrollback = appendCapped(s.scrollback, buf[:n])
			s.pending = append(s.pending, buf[:n]...)
			s.mu.Unlock()
		}
		if err != nil {
			m.finish(s)
			return
		}
	}
}

// flush pushes whatever the pump has accumulated, at most once per tick.
func (m *Manager) flush(s *session) {
	t := time.NewTicker(flushInterval)
	defer t.Stop()
	for range t.C {
		s.mu.Lock()
		if s.done {
			s.mu.Unlock()
			return
		}
		chunk := s.pending
		s.pending = nil
		s.mu.Unlock()

		if len(chunk) > 0 {
			m.push(s, Output{SessionID: s.id, Data: chunk})
		}
	}
}

// finish drains what is left, reports the exit, and forgets the session.
func (m *Manager) finish(s *session) {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.done = true
	chunk := s.pending
	s.pending = nil
	s.mu.Unlock()

	if len(chunk) > 0 {
		m.push(s, Output{SessionID: s.id, Data: chunk})
	}

	// Wait rather than read the exit status off the pty error: the PTY closing
	// and the child being reaped are different events, and only Wait knows the
	// code. It also stops the shell becoming a zombie for the life of the host.
	code := 0
	if err := s.cmd.Wait(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else {
			code = -1
		}
	}
	_ = s.pty.Close()

	m.mu.Lock()
	delete(m.sessions, s.id)
	m.mu.Unlock()

	m.log.Info("terminal: session closed", "session", s.id, "exit", code)
	m.push(s, Exit{SessionID: s.id, ExitCode: code})
	// The tab is gone on every client, not just the ones watching this shell.
	m.changed(s.cwd)
}

// push fans one message out to the attached devices. A device the transport
// cannot reach is dropped rather than retried: it will attach again and get the
// scrollback, which is a better answer than a queue that grows forever.
func (m *Manager) push(s *session, ev emit.Event) {
	s.mu.Lock()
	watchers := make([]device.ID, 0, len(s.watchers))
	for id := range s.watchers {
		watchers = append(watchers, id)
	}
	s.mu.Unlock()

	for _, id := range watchers {
		if err := m.emit.To(id, ev); err != nil {
			s.mu.Lock()
			delete(s.watchers, id)
			s.mu.Unlock()
		}
	}
}

// info is locked because geometry changes under a resize while a list is being
// built, and the two run on different goroutines.
func (s *session) info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Info{
		SessionID: s.id, Cwd: s.cwd, Shell: s.shell, Cols: s.cols, Rows: s.rows,
		PID: pidOf(s.cmd), CreatedAt: s.createdAt.UnixMilli(),
	}
}

func pidOf(cmd *exec.Cmd) int {
	if cmd.Process == nil {
		return 0
	}
	return cmd.Process.Pid
}

// appendCapped keeps the tail. Reallocating the whole buffer on every read
// would be quadratic on a chatty session, so it only copies once the cap is hit.
func appendCapped(buf, next []byte) []byte {
	buf = append(buf, next...)
	if len(buf) > scrollbackMax {
		buf = append(buf[:0], buf[len(buf)-scrollbackMax:]...)
	}
	return buf
}

func clamp(n, fallback int) int {
	if n <= 0 {
		return fallback
	}
	return min(n, maxDim)
}
