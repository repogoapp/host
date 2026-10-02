// Package actions runs the commands a project declares in its actions.json. A
// caller names an action, never a command, so a phone only pulls triggers the
// repo's author wired up. Detached runs return a run id and discard output.
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
	"sync"
	"syscall"
	"time"

	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/files"
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

	// MaxRunLife caps how long any run may live, declared timeout or not.
	MaxRunLife = 10 * time.Minute
)

var (
	ErrMalformed     = errkind.New(errkind.Invalid, "actions: actions.json is not valid")
	ErrUnknownAction = errkind.New(errkind.NotFound, "actions: no such action")
	ErrUnknownRun    = errkind.New(errkind.NotFound, "actions: no such run")
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

type Service struct {
	paths files.Container
	log   *slog.Logger

	mu   sync.Mutex
	runs map[string]context.CancelFunc
}

func New(paths files.Container, log *slog.Logger) *Service {
	return &Service{paths: paths, log: log, runs: map[string]context.CancelFunc{}}
}

// List reads a project's root actions.json. A project without one is normal
// and answers an empty list; a file that will not parse is the caller's to
// hear about, because the fix is editing the file.
func (s *Service) List(project string) ([]Action, error) {
	dir, err := s.paths.Contain(project)
	if err != nil {
		return nil, err
	}
	actions, err := load(dir)
	if err != nil {
		return nil, err
	}
	if actions == nil {
		actions = []Action{}
	}
	return actions, nil
}

// Run executes one blocking action and answers with its captured output.
func (s *Service) Run(ctx context.Context, project, name string) (Result, error) {
	dir, action, err := s.resolve(project, name)
	if err != nil {
		return Result{}, err
	}

	timeout := action.life(DefaultRunTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cwd, err := s.resolveCwd(dir, action.Cwd)
	if err != nil {
		return Result{}, err
	}

	start := time.Now()
	cmd := command(ctx, action.Cmd, cwd)
	stdout := &cappedBuffer{limit: MaxOutputBytes}
	stderr := &cappedBuffer{limit: MaxOutputBytes}
	cmd.Stdout, cmd.Stderr = stdout, stderr

	err = cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return Result{}, fmt.Errorf("actions: %s timed out after %s", name, timeout)
	}

	result := Result{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		DurationMs: time.Since(start).Milliseconds(),
	}
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		result.ExitCode = exit.ExitCode()
	default:
		return Result{}, err
	}
	return result, nil
}

// Start launches one detached action and answers immediately with a run id.
// Output is discarded; a watchdog reclaims the process at its declared
// timeout, capped at MaxRunLife.
func (s *Service) Start(project, name string) (string, error) {
	dir, action, err := s.resolve(project, name)
	if err != nil {
		return "", err
	}
	cwd, err := s.resolveCwd(dir, action.Cwd)
	if err != nil {
		return "", err
	}

	// Outlives the call that started it; the watchdog and Stop end it.
	ctx, cancel := context.WithTimeout(context.Background(), action.life(MaxRunLife))
	cmd := command(ctx, action.Cmd, cwd)
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = nil, nil
	if err := cmd.Start(); err != nil {
		cancel()
		return "", err
	}

	id := newRunID()
	s.mu.Lock()
	s.runs[id] = cancel
	s.mu.Unlock()

	go func() {
		err := cmd.Wait()
		cancel()
		s.mu.Lock()
		delete(s.runs, id)
		s.mu.Unlock()
		s.log.Info("detached action finished", "action", name, "run", id, "err", err)
	}()

	return id, nil
}

// Stop cancels one detached run; ErrUnknownRun covers one that already finished.
func (s *Service) Stop(runID string) error {
	s.mu.Lock()
	cancel := s.runs[runID]
	s.mu.Unlock()
	if cancel == nil {
		return ErrUnknownRun
	}
	cancel()
	return nil
}

// Running is how many detached runs are still going.
func (s *Service) Running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs)
}

// Shutdown reclaims every detached run this service started.
func (s *Service) Shutdown() {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(s.runs))
	for _, cancel := range s.runs {
		cancels = append(cancels, cancel)
	}
	s.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

// resolve re-reads the file and finds the named action. Re-read on every run,
// deliberately: the file the user just edited is the file that runs.
func (s *Service) resolve(project, name string) (string, Action, error) {
	dir, err := s.paths.Contain(project)
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
	return s.paths.Contain(filepath.Join(dir, cwd))
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

// command builds the shell invocation every run shares: the user's login
// shell, so an action sees the PATH the user's own terminal would.
func command(ctx context.Context, cmdline, cwd string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, terminal.LoginShell(), "-l", "-c", cmdline)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	// Its own process group, so cancelling kills the action's children too —
	// a `bun run dev` is a tree, not a process.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	}
	cmd.WaitDelay = 2 * time.Second
	return cmd
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
