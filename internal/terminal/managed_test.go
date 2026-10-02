package terminal_test

import (
	"testing"
	"time"

	"github.com/repogo/host/internal/terminal"
)

func TestRunReturnsTheCommandsExitCode(t *testing.T) {
	dir := t.TempDir()
	m := terminal.New(roots{dir}, newCollector().emitter(), quiet())
	t.Cleanup(m.Shutdown)

	code, err := m.Run(terminal.Spec{
		Managed: "env:setup", Project: dir, Dir: dir,
		Env: map[string]string{"REPOGO_TEST_CODE": "7"}, Cmd: `exit "$REPOGO_TEST_CODE"`,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit code = %d, want 7 from the spec's environment", code)
	}
	if id := m.FindManaged("env:setup"); id != "" {
		t.Fatalf("a finished setup is still found as %s", id)
	}
}

// A service lists under its project like any tab, starts once per id, and
// reads as stopped the moment its owner stops it.
func TestManagedServiceIsListedFoundAndStopped(t *testing.T) {
	dir := t.TempDir()
	m := terminal.New(roots{dir}, newCollector().emitter(), quiet())
	t.Cleanup(m.Shutdown)

	exited := make(chan int, 1)
	spec := terminal.Spec{Managed: "env:web", Project: dir, Dir: dir, Cmd: "sleep 60"}
	info, running, err := m.Start(spec, func(code int) { exited <- code })
	if err != nil || running {
		t.Fatalf("start: running=%v err=%v", running, err)
	}
	again, running, err := m.Start(spec, nil)
	if err != nil || !running || again.SessionID != info.SessionID {
		t.Fatalf("second start = %+v running=%v err=%v, want the same session", again, running, err)
	}
	if got := m.List(dir); len(got) != 1 || got[0].SessionID != info.SessionID {
		t.Fatalf("list = %+v, want the managed session", got)
	}
	if ids := m.Managed("env:"); len(ids) != 1 || ids[0] != "env:web" {
		t.Fatalf("managed = %v", ids)
	}
	// A dev server is not the user's shell: it neither holds an update back
	// nor uses up the shells Create allows.
	if n := m.Open(); n != 0 {
		t.Fatalf("Open = %d with only a managed session", n)
	}

	if !m.StopManaged("env:web") {
		t.Fatal("stop found nothing")
	}
	if id := m.FindManaged("env:web"); id != "" {
		t.Fatal("a stopped service is still found")
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("onExit never ran")
	}
}

// Closing or detaching from a session that already ended races its exit, so
// both succeed.
func TestCloseAndDetachAreIdempotent(t *testing.T) {
	m := terminal.New(roots{t.TempDir()}, newCollector().emitter(), quiet())
	if err := m.Close("gone"); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := m.Detach("phone", "gone"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if err := m.Input("gone", nil); err == nil {
		t.Fatal("input to a missing session succeeded")
	}
}
