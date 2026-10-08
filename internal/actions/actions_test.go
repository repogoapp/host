package actions

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/store"
	"github.com/repogo/host/internal/terminal"
	"github.com/repogo/host/internal/testwait"
)

// containRoot contains to one root, the way files.Service does for real.
type containRoot string

func (c containRoot) Contain(path string) (string, error) {
	full, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	root := string(c)
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", errors.New("outside root")
	}
	return full, nil
}

// harness is a service on a temporary state.db and real terminals, keeping
// every snapshot it announces.
type harness struct {
	*Service
	dir   string
	db    *store.Store
	terms *terminal.Manager

	mu    sync.Mutex
	snaps []Snapshot
}

type nopTransport struct{}

func (nopTransport) Send(device.ID, string, []byte) error { return nil }

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &harness{dir: dir, db: db, terms: terminal.New(containRoot(dir), emit.New(nopTransport{}, log), log)}
	t.Cleanup(h.terms.Shutdown)
	h.Service = h.open(t)
	return h
}

// open builds a service on the harness's store, as a host starting does.
func (h *harness) open(t *testing.T) *Service {
	t.Helper()
	s, err := New(Deps{
		Paths: containRoot(h.dir), Store: h.db, Terminals: h.terms, Logs: filepath.Join(h.dir, ".logs"),
		Changed: func(snap Snapshot) {
			h.mu.Lock()
			h.snaps = append(h.snaps, snap)
			h.mu.Unlock()
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	return s
}

func (h *harness) last() Snapshot {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.snaps[len(h.snaps)-1]
}

// status is the run's row as kept.
func (h *harness) status(t *testing.T, id string) store.ActionRun {
	t.Helper()
	run, ok, err := h.db.ActionRun(id)
	if err != nil || !ok {
		t.Fatalf("run %s: %v, found %v", id, err, ok)
	}
	return run
}

func newTestService(t *testing.T) (*Service, string) {
	h := newHarness(t)
	return h.Service, h.dir
}

func write(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "actions.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListWithoutFileIsEmptyNotAnError(t *testing.T) {
	s, dir := newTestService(t)
	snap, err := s.List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	actions := snap.Actions
	if len(actions) != 0 {
		t.Fatalf("got %d actions, want 0", len(actions))
	}
}

func TestListParsesTheRepogoSchema(t *testing.T) {
	s, dir := newTestService(t)
	write(t, dir, `{"actions":[
		{"name":"build","label":"Build","cmd":"make","group":"CI","timeoutMs":5000},
		{"name":"dev","cmd":"make dev","detach":true,"island":true},
		{"name":"","cmd":"dropped"},
		{"name":"dropped-too"}
	]}`)
	snap, err := s.List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	actions := snap.Actions
	if len(actions) != 2 {
		t.Fatalf("got %d actions, want 2 (rows without name or cmd drop)", len(actions))
	}
	if actions[0].Label != "Build" || actions[0].TimeoutMs != 5000 || !actions[1].Detach ||
		actions[0].Island || !actions[1].Island {
		t.Fatalf("schema did not round-trip: %+v", actions)
	}
}

func TestListMalformedIsErrMalformed(t *testing.T) {
	s, dir := newTestService(t)
	write(t, dir, `{"actions": not json`)
	if _, err := s.List(dir); !errors.Is(err, ErrMalformed) {
		t.Fatalf("got %v, want ErrMalformed", err)
	}
}

func TestRunCapturesOutputAndExitCode(t *testing.T) {
	s, dir := newTestService(t)
	write(t, dir, `{"actions":[
		{"name":"hello","cmd":"echo out; echo err 1>&2"},
		{"name":"fails","cmd":"exit 3"}
	]}`)

	result, err := s.Run(context.Background(), "a", dir, "hello")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(result.Stdout, "out") || !strings.Contains(result.Stderr, "err") {
		t.Fatalf("output not captured: %+v", result)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", result.ExitCode)
	}

	// A non-zero exit is a result, not an error.
	result, err = s.Run(context.Background(), "a", dir, "fails")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 3 {
		t.Fatalf("exit = %d, want 3", result.ExitCode)
	}
}

func TestRunUnknownActionIsErrUnknownAction(t *testing.T) {
	s, dir := newTestService(t)
	write(t, dir, `{"actions":[{"name":"a","cmd":"true"}]}`)
	if _, err := s.Run(context.Background(), "a", dir, "nope"); !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("got %v, want ErrUnknownAction", err)
	}
}

func TestRunTimesOut(t *testing.T) {
	s, dir := newTestService(t)
	write(t, dir, `{"actions":[{"name":"slow","cmd":"sleep 30","timeoutMs":200}]}`)
	start := time.Now()
	if _, err := s.Run(context.Background(), "a", dir, "slow"); err == nil {
		t.Fatal("a timed-out run must be an error")
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout did not fire")
	}
}

func TestCwdCannotEscapeTheProject(t *testing.T) {
	s, dir := newTestService(t)
	write(t, dir, `{"actions":[
		{"name":"escape","cmd":"pwd","cwd":"../../.."},
		{"name":"abs","cmd":"pwd","cwd":"/tmp"}
	]}`)
	if _, err := s.Run(context.Background(), "a", dir, "escape"); err == nil {
		t.Fatal("a cwd above the project must be refused")
	}
	if _, err := s.Run(context.Background(), "a", dir, "abs"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("an absolute cwd answered %v, want ErrMalformed", err)
	}
}

// A detached run is a row from start to end, announced each time; a second
// start answers with the run already going, and Stop settles it as stopped.
func TestDetachedRunIsKeptAndStopped(t *testing.T) {
	h := newHarness(t)
	write(t, h.dir, `{"actions":[{"name":"bg","cmd":"echo hello; touch ran; sleep 60","detach":true}]}`)

	run, err := h.Start("phone", h.dir, "bg")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if run.Status != store.RunRunning || run.DeviceID != "phone" || run.Cmd == "" || run.PID == 0 {
		t.Fatalf("Start = %+v", run)
	}
	if snap := h.last(); len(snap.Runs) != 1 || snap.Runs[0].ID != run.ID {
		t.Fatalf("announced %+v, want the run", snap.Runs)
	}
	again, err := h.Start("other", h.dir, "bg")
	if err != nil || again.ID != run.ID {
		t.Fatalf("second Start = %+v, %v; want the running one", again, err)
	}

	testwait.For(t, "the command to run", func() bool {
		_, err := os.Stat(filepath.Join(h.dir, "ran"))
		return err == nil
	})
	if out, err := h.Output(run.ID); err != nil || !strings.Contains(out.Text, "hello") {
		t.Fatalf("Output = %+v, %v", out, err)
	}
	if err := h.Stop(run.ID); err != nil {
		t.Fatal(err)
	}
	testwait.For(t, "the run to settle", func() bool { return h.status(t, run.ID).Status == store.RunStopped })
	if snap := h.last(); len(snap.Runs) != 1 || snap.Runs[0].Status != store.RunStopped {
		t.Fatalf("last snapshot %+v, want the stopped run", snap.Runs)
	}
	if err := h.Stop("not-a-run"); !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("unknown run: %v", err)
	}
}

func TestDetachedExitKeepsItsCode(t *testing.T) {
	h := newHarness(t)
	write(t, h.dir, `{"actions":[{"name":"bg","cmd":"exit 4","detach":true}]}`)
	run, err := h.Start("phone", h.dir, "bg")
	if err != nil {
		t.Fatal(err)
	}
	testwait.For(t, "the run to settle", func() bool { return h.status(t, run.ID).Status != store.RunRunning })
	got := h.status(t, run.ID)
	if got.Status != store.RunExited || got.ExitCode == nil || *got.ExitCode != 4 || got.EndedAt == 0 {
		t.Fatalf("settled %+v, want exited 4", got)
	}
}

// A terminal run settles when its command ends, and its tab stays open with
// the output; closing a tab while the command runs settles it as stopped.
func TestTerminalRunSettlesFromItsTab(t *testing.T) {
	h := newHarness(t)
	write(t, h.dir, `{"actions":[
		{"name":"once","cmd":"exit 5","terminal":true},
		{"name":"server","cmd":"sleep 60","terminal":true}
	]}`)
	once, err := h.Start("phone", h.dir, "once")
	if err != nil {
		t.Fatal(err)
	}
	if once.Mode != ModeTerminal || once.SessionID == "" {
		t.Fatalf("Start = %+v, want a terminal run", once)
	}
	testwait.For(t, "the command to end", func() bool { return h.status(t, once.ID).Status != store.RunRunning })
	if got := h.status(t, once.ID); got.Status != store.RunExited || got.ExitCode == nil || *got.ExitCode != 5 {
		t.Fatalf("settled %+v, want exited 5", got)
	}
	if !hasTab(h.terms, h.dir, once.SessionID) {
		t.Error("the tab closed with its command")
	}

	server, err := h.Start("phone", h.dir, "server")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.terms.Close(server.SessionID); err != nil {
		t.Fatal(err)
	}
	testwait.For(t, "the run to settle", func() bool { return h.status(t, server.ID).Status == store.RunStopped })
}

func hasTab(m *terminal.Manager, dir, id string) bool {
	for _, info := range m.List(dir) {
		if info.SessionID == id {
			return true
		}
	}
	return false
}

// A host starting up adopts a detached run whose process is still the same,
// and marks every other run left running as lost.
func TestRestartAdoptsTheSameProcessAndLosesTheRest(t *testing.T) {
	h := newHarness(t)
	write(t, h.dir, `{"actions":[{"name":"bg","cmd":"sleep 60","detach":true}]}`)
	run, err := h.Start("phone", h.dir, "bg")
	if err != nil {
		t.Fatal(err)
	}
	h.Shutdown() // the host goes; the run does not
	gone := store.ActionRun{ID: "gone", Path: h.dir, Action: "bg", Cmd: "x", Mode: ModeDetached,
		PID: run.PID, PIDStarted: run.PIDStarted + 1, StartedAt: 1}
	if err := h.db.InsertActionRun(gone); err != nil {
		t.Fatal(err)
	}

	next := h.open(t)
	if got := h.status(t, "gone"); got.Status != store.RunLost || got.ExitCode != nil {
		t.Fatalf("a reused pid was taken for the run: %+v", got)
	}
	if got := h.status(t, run.ID); got.Status != store.RunRunning {
		t.Fatalf("the live run was not adopted: %+v", got)
	}
	if err := next.Stop(run.ID); err != nil {
		t.Fatal(err)
	}
	testwait.For(t, "the adopted run to settle", func() bool {
		next.watch()
		return h.status(t, run.ID).Status == store.RunStopped
	})
}

// Each action keeps its newest ended runs and drops the rest with their logs.
func TestEndedRunsArePruned(t *testing.T) {
	h := newHarness(t)
	write(t, h.dir, `{"actions":[{"name":"a","cmd":"true"}]}`)
	var first string
	for i := range KeepRuns + 2 {
		if _, err := h.Run(context.Background(), "phone", h.dir, "a"); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = h.last().Runs[0].ID
		}
	}
	if _, ok, _ := h.db.ActionRun(first); ok {
		t.Error("the oldest run was kept")
	}
	if runs := h.last().Runs; len(runs) != 1 || runs[0].Status != store.RunExited {
		t.Fatalf("snapshot runs %+v, want the newest ended one", runs)
	}
}

// A file far past the cap is refused without reading it whole.
func TestLoadRefusesAnOversizedFile(t *testing.T) {
	s, dir := newTestService(t)
	write(t, dir, `{"actions":[]}`+strings.Repeat(" ", MaxFileBytes))
	if _, err := s.List(dir); !errors.Is(err, ErrMalformed) {
		t.Fatalf("oversized file: %v", err)
	}
}

// A declared timeout is honoured up to MaxRunLife, blocking or detached.
func TestRunLifeIsCapped(t *testing.T) {
	for _, c := range []struct {
		ms   int
		want time.Duration
	}{
		{0, DefaultRunTimeout},
		{1500, 1500 * time.Millisecond},
		{int(24 * time.Hour / time.Millisecond), MaxRunLife},
	} {
		if got := (Action{TimeoutMs: c.ms}).life(DefaultRunTimeout); got != c.want {
			t.Errorf("timeoutMs %d: life %v, want %v", c.ms, got, c.want)
		}
	}
}
