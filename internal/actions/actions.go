// Package actions runs the commands a project declares in its actions.json. A
// caller names an action, never a command, so a phone only pulls triggers the
// repo's author wired up. Every run is a row in state.db, announced to every
// device as the project's snapshot, and a detached run outlives the host.
package actions

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/procscan"
	"github.com/repogo/host/internal/store"
	"github.com/repogo/host/internal/terminal"
)

const (
	// MaxFileBytes bounds one actions.json. Fifty actions of prose fit in a
	// few kilobytes; past this the file is not an actions file.
	MaxFileBytes = 1 << 20

	// MaxActions bounds one file's rows, matching the clients' cap.
	MaxActions = 20

	// MaxOutputBytes bounds each captured stream of a blocking run. The
	// answer travels to a phone.
	MaxOutputBytes = 1 << 20

	// DefaultRunTimeout is a blocking run with no declared timeoutMs.
	DefaultRunTimeout = 30 * time.Second

	// MaxRunLife caps how long a blocking run may live, declared timeout or
	// not. A detached or terminal run lives until it ends or is stopped.
	MaxRunLife = 10 * time.Minute

	// KeepRuns is how many ended runs each action keeps, with their logs.
	KeepRuns = 10

	// MaxLogBytes is the tail of a detached run's output that actions.output
	// answers with; a log past trimLogBytes is cut back to it.
	MaxLogBytes  = 256 << 10
	trimLogBytes = 1 << 20

	// watchEvery is how often a run adopted after a restart, which this
	// process cannot wait on, is checked for having ended.
	watchEvery = 2 * time.Second
	// stopGrace is how long a stopped run has between SIGTERM and SIGKILL.
	stopGrace = 5 * time.Second
)

var (
	ErrMalformed     = errkind.New(errkind.Invalid, "actions: actions.json is not valid")
	ErrUnknownAction = errkind.New(errkind.NotFound, "actions: no such action")
	ErrUnknownRun    = errkind.New(errkind.NotFound, "actions: no such run")
	ErrClosing       = errkind.New(errkind.Unavailable, "actions: the host is shutting down")
)

// Run modes: how a run's process is held.
const (
	ModeBlocking = "blocking"
	ModeDetached = "detached"
	ModeTerminal = "terminal"
)

// Action is one row of actions.json. JSON tags are the repogo v1 schema —
// camelCase where it was camelCase — so existing files parse unchanged.
type Action struct {
	Name        string `json:"name"`
	Label       string `json:"label,omitempty"`
	Cmd         string `json:"cmd"`
	Cwd         string `json:"cwd,omitempty"`
	Group       string `json:"group,omitempty"`
	Description string `json:"description,omitempty"`
	TimeoutMs   int    `json:"timeoutMs,omitempty"`
	Detach      bool   `json:"detach,omitempty"`
	// Terminal runs the action in a terminal tab of its own, which stays open
	// with the output once the command ends.
	Terminal bool `json:"terminal,omitempty"`
	// Island pins the action to the chat composer island. The host only
	// carries it through; runs are the same either way.
	Island bool `json:"island,omitempty"`
}

// Result is what a blocking run answers with. A non-zero exit is a result,
// not an error — the row turns red, the connection does not.
type Result struct {
	ExitCode   int    `json:"exit_code"`
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMs int64  `json:"duration_ms"`
}

// Output is the tail of a detached run's log.
type Output struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

// Store is the runs table in state.db.
type Store interface {
	InsertActionRun(store.ActionRun) error
	SettleActionRun(id, status string, endedAt int64, exitCode *int) (bool, error)
	ActionRun(id string) (store.ActionRun, bool, error)
	ActionRuns(path string) ([]store.ActionRun, error)
	RunningActionRuns() ([]store.ActionRun, error)
	PruneActionRuns(path, action string, keep int) ([]string, error)
}

// Terminals opens a terminal run's tab and closes it.
type Terminals interface {
	StartCommand(terminal.Command, func(code int, finished bool)) (terminal.Info, error)
	Close(id string) error
}

// Deps is everything the service touches. Every field is required.
type Deps struct {
	Paths     files.Container
	Store     Store
	Terminals Terminals
	// Changed sends a project's snapshot to every device.
	Changed func(Snapshot)
	// Logs is the folder detached runs write their output to.
	Logs string
	Log  *slog.Logger
}

// live is a run this host holds: its row and how to stop it.
type live struct {
	run      store.ActionRun
	stop     func(store.ActionRun)
	stopping bool
	// adopted is a detached run found running at startup: not this process's
	// child, so its end is noticed by watching, without an exit code.
	adopted bool
}

type Service struct {
	d Deps

	mu       sync.Mutex
	live     map[string]*live
	revision int64
	closing  bool
	// blocking is the blocking runs still to settle; Shutdown waits for them.
	blocking sync.WaitGroup
}

// New refuses Deps with a field unset, then settles the runs a previous host
// left marked running: a detached one whose process is still the same is
// adopted, and every other is lost.
func New(d Deps) (*Service, error) {
	var missing []string
	for _, f := range []struct {
		name  string
		unset bool
	}{
		{"Paths", d.Paths == nil}, {"Store", d.Store == nil}, {"Terminals", d.Terminals == nil},
		{"Changed", d.Changed == nil}, {"Logs", d.Logs == ""}, {"Log", d.Log == nil},
	} {
		if f.unset {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("actions: missing %s", strings.Join(missing, ", "))
	}
	if err := os.MkdirAll(d.Logs, 0o700); err != nil {
		return nil, err
	}
	s := &Service{d: d, live: map[string]*live{}, revision: time.Now().UnixMilli()}
	runs, err := d.Store.RunningActionRuns()
	if err != nil {
		return nil, err
	}
	for _, r := range runs {
		if r.Mode == ModeDetached && sameProcess(r) {
			s.live[r.ID] = &live{run: r, adopted: true, stop: stopGroup}
			d.Log.Info("action run adopted", "action", r.Action, "run", r.ID, "pid", r.PID)
			continue
		}
		if _, err := d.Store.SettleActionRun(r.ID, store.RunLost, 0, nil); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// List is a project's actions and the runs they show.
func (s *Service) List(project string) (Snapshot, error) {
	dir, err := s.d.Paths.Contain(project)
	if err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked(dir)
}

// Run executes one blocking action and answers with its captured output.
func (s *Service) Run(ctx context.Context, caller device.ID, project, name string) (Result, error) {
	dir, action, err := s.resolve(project, name)
	if err != nil {
		return Result{}, err
	}
	cwd, err := s.resolveCwd(dir, action.Cwd)
	if err != nil {
		return Result{}, err
	}
	timeout := action.life(DefaultRunTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := command(ctx, action.Cmd, cwd)
	stdout := &cappedBuffer{limit: MaxOutputBytes}
	stderr := &cappedBuffer{limit: MaxOutputBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	start := time.Now()
	run, err := s.begin(caller, dir, action, ModeBlocking, func(run *store.ActionRun) (func(store.ActionRun), error) {
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		run.PID = cmd.Process.Pid
		return func(store.ActionRun) { cancel() }, nil
	})
	if err != nil {
		return Result{}, err
	}
	s.blocking.Add(1)
	defer s.blocking.Done()

	err = cmd.Wait()
	timedOut := ctx.Err() == context.DeadlineExceeded
	code := 0
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		code = -1
	}
	s.mu.Lock()
	if l := s.live[run.ID]; l != nil {
		delete(s.live, run.ID)
		status, exitCode := outcome(l.stopping, code, true)
		if timedOut {
			status, exitCode = store.RunTimedOut, nil
		}
		s.settleLocked(l.run, status, exitCode)
	}
	s.mu.Unlock()

	if timedOut {
		return Result{}, fmt.Errorf("actions: %s timed out after %s", name, timeout)
	}
	if err != nil && exit == nil {
		return Result{}, err
	}
	return Result{
		ExitCode: code, Stdout: stdout.String(), Stderr: stderr.String(),
		DurationMs: time.Since(start).Milliseconds(),
	}, nil
}

// Start launches an action that outlives the call: in a terminal tab when it
// asks for one, else detached with its output in a log. An action already
// running answers with that run rather than starting a second.
func (s *Service) Start(caller device.ID, project, name string) (store.ActionRun, error) {
	dir, action, err := s.resolve(project, name)
	if err != nil {
		return store.ActionRun{}, err
	}
	cwd, err := s.resolveCwd(dir, action.Cwd)
	if err != nil {
		return store.ActionRun{}, err
	}
	if action.Terminal {
		return s.begin(caller, dir, action, ModeTerminal, func(run *store.ActionRun) (func(store.ActionRun), error) {
			id := run.ID
			info, err := s.d.Terminals.StartCommand(terminal.Command{Dir: cwd, Cmd: action.Cmd},
				func(code int, finished bool) { s.ended(id, code, finished) })
			if err != nil {
				return nil, err
			}
			run.PID, run.SessionID = info.PID, info.SessionID
			return func(store.ActionRun) { _ = s.d.Terminals.Close(info.SessionID) }, nil
		})
	}
	return s.begin(caller, dir, action, ModeDetached, func(run *store.ActionRun) (func(store.ActionRun), error) {
		out, err := os.OpenFile(s.logPath(run.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		defer out.Close()
		cmd := exec.Command(terminal.LoginShell(), "-l", "-c", action.Cmd)
		prepare(cmd, cwd)
		cmd.Stdout, cmd.Stderr = out, out
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		run.PID = cmd.Process.Pid
		id := run.ID
		go func() {
			code := 0
			var exit *exec.ExitError
			if err := cmd.Wait(); errors.As(err, &exit) {
				code = exit.ExitCode()
			} else if err != nil {
				code = -1
			}
			s.ended(id, code, true)
		}()
		return stopGroup, nil
	})
}

// begin starts a run through spawn and keeps it: the row is written right
// after the process starts, and a row that cannot be written stops it, so
// nothing runs unrecorded.
func (s *Service) begin(caller device.ID, dir string, action Action, mode string,
	spawn func(*store.ActionRun) (func(store.ActionRun), error)) (store.ActionRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return store.ActionRun{}, ErrClosing
	}
	if mode != ModeBlocking {
		for _, l := range s.live {
			if l.run.Path == dir && l.run.Action == action.Name {
				return l.run, nil
			}
		}
	}
	run := store.ActionRun{
		ID: newRunID(), Path: dir, Action: action.Name, Cmd: action.Cmd, Mode: mode,
		DeviceID: string(caller), Status: store.RunRunning, StartedAt: time.Now().UnixMilli(),
	}
	stop, err := spawn(&run)
	if err != nil {
		return store.ActionRun{}, err
	}
	run.PIDStarted, _ = procscan.Started(run.PID)
	if err := s.d.Store.InsertActionRun(run); err != nil {
		stop(run)
		return store.ActionRun{}, err
	}
	s.live[run.ID] = &live{run: run, stop: stop}
	s.d.Log.Info("action started", "action", action.Name, "run", run.ID, "mode", mode, "pid", run.PID)
	s.announceLocked(dir)
	return run, nil
}

// ended settles a detached or terminal run. A host shutting down leaves its
// rows running, for the next one to adopt or lose.
func (s *Service) ended(id string, code int, finished bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.live[id]
	if l == nil || s.closing {
		return
	}
	delete(s.live, id)
	status, exitCode := outcome(l.stopping, code, finished)
	s.settleLocked(l.run, status, exitCode)
}

// outcome is how a run ended: stopped when asked to, interrupted or killed,
// else exited with its code.
func outcome(stopping bool, code int, finished bool) (string, *int) {
	switch {
	case !finished:
		return store.RunStopped, nil
	case code < 0:
		return store.RunStopped, nil
	case stopping || code == 130 || code == 143:
		return store.RunStopped, &code
	}
	return store.RunExited, &code
}

func (s *Service) settleLocked(run store.ActionRun, status string, exitCode *int) {
	if _, err := s.d.Store.SettleActionRun(run.ID, status, time.Now().UnixMilli(), exitCode); err != nil {
		s.d.Log.Warn("action run not settled", "run", run.ID, "err", err)
	}
	s.d.Log.Info("action ended", "action", run.Action, "run", run.ID, "status", status)
	pruned, err := s.d.Store.PruneActionRuns(run.Path, run.Action, KeepRuns)
	if err != nil {
		s.d.Log.Warn("action runs not pruned", "action", run.Action, "err", err)
	}
	for _, id := range pruned {
		_ = os.Remove(s.logPath(id))
	}
	s.announceLocked(run.Path)
}

// Stop ends a run this host holds; ErrUnknownRun covers one already ended.
// The run settles as stopped once its process is gone.
func (s *Service) Stop(runID string) error {
	s.mu.Lock()
	l := s.live[runID]
	if l == nil {
		s.mu.Unlock()
		return ErrUnknownRun
	}
	l.stopping = true
	stop, run := l.stop, l.run
	s.mu.Unlock()
	stop(run)
	return nil
}

// Output is the tail of a detached run's log; other runs have none here.
func (s *Service) Output(runID string) (Output, error) {
	run, ok, err := s.d.Store.ActionRun(runID)
	if err != nil {
		return Output{}, err
	}
	if !ok {
		return Output{}, ErrUnknownRun
	}
	if run.Mode != ModeDetached {
		return Output{}, nil
	}
	f, err := os.Open(s.logPath(runID))
	if errors.Is(err, os.ErrNotExist) {
		return Output{}, nil
	}
	if err != nil {
		return Output{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Output{}, err
	}
	from := max(info.Size()-MaxLogBytes, 0)
	raw, err := io.ReadAll(io.NewSectionReader(f, from, info.Size()-from))
	if err != nil {
		return Output{}, err
	}
	return Output{Text: string(raw), Truncated: from > 0}, nil
}

// Running is how many blocking runs are going: what a host update waits for.
// Detached runs outlive the host, and terminal runs count as terminals.
func (s *Service) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, l := range s.live {
		if l.run.Mode == ModeBlocking {
			n++
		}
	}
	return n
}

// Watch settles adopted runs once their process is gone and keeps detached
// logs to their tail, until ctx is cancelled.
func (s *Service) Watch(ctx context.Context) {
	t := time.NewTicker(watchEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.watch()
		}
	}
}

func (s *Service) watch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, l := range s.live {
		if l.adopted && !sameProcess(l.run) {
			delete(s.live, id)
			status := store.RunExited
			if l.stopping {
				status = store.RunStopped
			}
			s.settleLocked(l.run, status, nil)
			continue
		}
		if l.run.Mode == ModeDetached {
			s.trimLog(id)
		}
	}
}

// trimLog cuts a log past trimLogBytes back to its tail. The process writes
// with O_APPEND, so it carries on at the new end.
func (s *Service) trimLog(id string) {
	path := s.logPath(id)
	info, err := os.Stat(path)
	if err != nil || info.Size() <= trimLogBytes {
		return
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer f.Close()
	tail := make([]byte, MaxLogBytes)
	n, _ := f.ReadAt(tail, info.Size()-MaxLogBytes)
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt(tail[:n], 0)
	}
}

// Shutdown stops blocking runs and waits for them to settle. Detached runs
// keep going for the next host to adopt; terminal tabs close with the
// terminals.
func (s *Service) Shutdown() {
	s.mu.Lock()
	s.closing = true
	for _, l := range s.live {
		if l.run.Mode == ModeBlocking {
			l.stopping = true
			l.stop(l.run)
		}
	}
	s.mu.Unlock()
	s.blocking.Wait()
}

func (s *Service) snapshotLocked(dir string) (Snapshot, error) {
	actions, err := load(dir)
	if err != nil {
		return Snapshot{}, err
	}
	if actions == nil {
		actions = []Action{}
	}
	runs, err := s.d.Store.ActionRuns(dir)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{Path: dir, Revision: s.revision, Actions: actions, Runs: runs}, nil
}

// announceLocked sends the project's snapshot with a newer revision. Under mu,
// so snapshots leave in the order their changes were made.
func (s *Service) announceLocked(dir string) {
	s.revision = max(s.revision+1, time.Now().UnixMilli())
	snap, err := s.snapshotLocked(dir)
	if err != nil {
		s.d.Log.Warn("actions not announced", "path", dir, "err", err)
		return
	}
	s.d.Changed(snap)
}

func (s *Service) logPath(id string) string { return filepath.Join(s.d.Logs, id+".log") }

// sameProcess is whether the run's process is still the one it started:
// the pid alone may have been given to another since.
func sameProcess(r store.ActionRun) bool {
	if r.PIDStarted == 0 {
		return false
	}
	started, ok := procscan.Started(r.PID)
	return ok && started == r.PIDStarted
}

// stopGroup ends a detached run's process group: SIGTERM, then SIGKILL after
// stopGrace. It checks the process is still the run's before each signal.
func stopGroup(r store.ActionRun) {
	if !sameProcess(r) {
		return
	}
	_ = syscall.Kill(-r.PID, syscall.SIGTERM)
	time.AfterFunc(stopGrace, func() {
		if sameProcess(r) {
			_ = syscall.Kill(-r.PID, syscall.SIGKILL)
		}
	})
}

// resolve re-reads the file and finds the named action. Re-read on every run,
// deliberately: the file the user just edited is the file that runs.
func (s *Service) resolve(project, name string) (string, Action, error) {
	dir, err := s.d.Paths.Contain(project)
	if err != nil {
		return "", Action{}, err
	}
	actions, err := load(dir)
	if err != nil {
		return "", Action{}, err
	}
	for _, a := range actions {
		if a.Name == name {
			return dir, a, nil
		}
	}
	return "", Action{}, fmt.Errorf("%w: %s", ErrUnknownAction, name)
}

// resolveCwd resolves an action's workspace-relative cwd through the same
// containment as the project itself, so `"cwd": "../.."` buys nothing.
func (s *Service) resolveCwd(dir, cwd string) (string, error) {
	if cwd == "" {
		return dir, nil
	}
	if filepath.IsAbs(cwd) {
		return "", fmt.Errorf("%w: cwd must be relative to the project", ErrMalformed)
	}
	return s.d.Paths.Contain(filepath.Join(dir, cwd))
}

// life is how long a run of a may take: its declared timeout, else fallback,
// never past MaxRunLife.
func (a Action) life(fallback time.Duration) time.Duration {
	if a.TimeoutMs > 0 {
		fallback = time.Duration(a.TimeoutMs) * time.Millisecond
	}
	return min(fallback, MaxRunLife)
}

func load(dir string) ([]Action, error) {
	f, err := os.Open(filepath.Join(dir, "actions.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// One byte past the cap is enough to know a file is too big.
	raw, err := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxFileBytes {
		return nil, fmt.Errorf("%w: file is larger than %d bytes", ErrMalformed, MaxFileBytes)
	}
	var file struct {
		Actions []Action `json:"actions"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrMalformed, err)
	}
	// Rows missing name or cmd are dropped rather than failing the file —
	// one half-typed entry should not hide the working ones.
	out := make([]Action, 0, len(file.Actions))
	for _, a := range file.Actions {
		if a.Name == "" || a.Cmd == "" {
			continue
		}
		out = append(out, a)
		if len(out) == MaxActions {
			break
		}
	}
	return out, nil
}

// command builds the shell invocation a blocking run uses: the user's login
// shell, so an action sees the PATH the user's own terminal would.
func command(ctx context.Context, cmdline, cwd string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, terminal.LoginShell(), "-l", "-c", cmdline)
	prepare(cmd, cwd)
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
}

// prepare gives a run its folder, the host's environment and a process group
// of its own, so stopping it stops its children too: a `bun run dev` is a
// tree, not a process.
func prepare(cmd *exec.Cmd, cwd string) {
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func newRunID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// cappedBuffer keeps the first limit bytes and drops the rest, because the
// answer travels to a phone and megabyte forty adds nothing.
type cappedBuffer struct {
	limit int
	buf   []byte
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.buf); room > 0 {
		if len(p) > room {
			b.buf = append(b.buf, p[:room]...)
		} else {
			b.buf = append(b.buf, p...)
		}
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return string(b.buf) }
