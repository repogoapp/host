package actions

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func newTestService(t *testing.T) (*Service, string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(containRoot(dir), log), dir
}

func write(t *testing.T, dir, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "actions.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestListWithoutFileIsEmptyNotAnError(t *testing.T) {
	s, dir := newTestService(t)
	actions, err := s.List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
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
	actions, err := s.List(dir)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
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

	result, err := s.Run(context.Background(), dir, "hello")
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
	result, err = s.Run(context.Background(), dir, "fails")
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
	if _, err := s.Run(context.Background(), dir, "nope"); !errors.Is(err, ErrUnknownAction) {
		t.Fatalf("got %v, want ErrUnknownAction", err)
	}
}

func TestRunTimesOut(t *testing.T) {
	s, dir := newTestService(t)
	write(t, dir, `{"actions":[{"name":"slow","cmd":"sleep 30","timeoutMs":200}]}`)
	start := time.Now()
	if _, err := s.Run(context.Background(), dir, "slow"); err == nil {
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
	if _, err := s.Run(context.Background(), dir, "escape"); err == nil {
		t.Fatal("a cwd above the project must be refused")
	}
	if _, err := s.Run(context.Background(), dir, "abs"); !errors.Is(err, ErrMalformed) {
		t.Fatalf("an absolute cwd answered %v, want ErrMalformed", err)
	}
}

func TestDetachedStartAndStop(t *testing.T) {
	s, dir := newTestService(t)
	marker := filepath.Join(dir, "ran")
	write(t, dir, `{"actions":[
		{"name":"bg","cmd":"touch ran; sleep 60","detach":true}
	]}`)

	id, err := s.Start(dir, "bg")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id == "" {
		t.Fatal("Start answered no run id")
	}

	testwait.For(t, "the detached command to run", func() bool {
		_, err := os.Stat(marker)
		return err == nil
	})

	if err := s.Stop(id); err != nil {
		t.Fatal(err)
	}
	testwait.For(t, "Stop to reclaim the run", func() bool { return s.Running() == 0 })

	if err := s.Stop("not-a-run"); !errors.Is(err, ErrUnknownRun) {
		t.Fatalf("unknown run: %v", err)
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
