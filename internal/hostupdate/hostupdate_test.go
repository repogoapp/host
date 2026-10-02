package hostupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/repogo/host/internal/release"
)

type turns struct {
	active  int
	guard   error
	guarded []bool // force of each GuardUpdate(true, …)
	cleared int
	stopped int
}

func (t *turns) GuardUpdate(enabled, force bool) error {
	if !enabled {
		t.cleared++
		return nil
	}
	if t.guard != nil {
		return t.guard
	}
	t.guarded = append(t.guarded, force)
	return nil
}
func (t *turns) ActiveChats() int { return t.active }
func (t *turns) StopAll(context.Context) error {
	t.stopped++
	t.active = 0
	return nil
}

type fixture struct {
	dir       string
	binary    string
	marker    string
	turns     *turns
	terminals int
	builds    int
	staged    int
	stageErr  error
	restarted chan struct{}
	u         *Updater
}

func newFixture(t *testing.T, version, latest string) *fixture {
	t.Helper()
	f := &fixture{dir: t.TempDir(), turns: &turns{}, restarted: make(chan struct{}, 1)}
	f.binary = filepath.Join(f.dir, "repogo")
	f.marker = filepath.Join(f.dir, "update.json")
	write(t, f.binary, "old")
	f.u = New(Deps{
		Version: version, Binary: f.binary, Marker: f.marker,
		Latest: func(context.Context) (release.Manifest, error) { return release.Manifest{Version: latest}, nil },
		Stage: func(_ context.Context, m release.Manifest, binary string) (string, error) {
			f.staged++
			if f.stageErr != nil {
				return "", f.stageErr
			}
			staged := filepath.Join(filepath.Dir(binary), ".repogo-update-test")
			return staged, os.WriteFile(staged, []byte("new "+m.Version), 0o755)
		},
		Turns:     f.turns,
		Terminals: func() int { return f.terminals },
		Actions:   func() int { return 0 },
		Builds:    func() int { return f.builds },
		Restart:   func() { f.restarted <- struct{}{} },
	})
	return f
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestDevBuildRefuses(t *testing.T) {
	f := newFixture(t, "dev", "0.2.0")
	if _, err := f.u.Update(context.Background(), true); !errors.Is(err, ErrDevBuild) {
		t.Fatalf("err %v, want ErrDevBuild", err)
	}
}

func TestCurrentHostChangesNothing(t *testing.T) {
	f := newFixture(t, "0.2.0", "0.2.0")
	got, err := f.u.Update(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.To != "" || got.From != "0.2.0" || f.staged != 0 {
		t.Fatalf("result %+v after %d downloads; want nothing to do", got, f.staged)
	}
}

// Work the update would stop comes back as a count, before any download, so
// the phone can ask first.
func TestBusyHostAsksBeforeStopping(t *testing.T) {
	f := newFixture(t, "0.1.0", "0.2.0")
	f.turns.active, f.terminals, f.builds = 1, 2, 3
	got, err := f.u.Update(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Busy == nil || *got.Busy != (Busy{Chats: 1, Terminals: 2, Builds: 3}) || got.To != "0.2.0" {
		t.Fatalf("result %+v, want busy 1 chat 2 terminals 3 builds", got)
	}
	if f.staged != 0 || len(f.turns.guarded) != 0 || read(t, f.binary) != "old" {
		t.Fatal("a refused update touched the host")
	}
}

func TestUpdateSwapsKeepsPreviousAndRestarts(t *testing.T) {
	f := newFixture(t, "0.1.0", "0.2.0")
	f.turns.active = 1
	got, err := f.u.Update(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Result{From: "0.1.0", To: "0.2.0"}) {
		t.Fatalf("result %+v", got)
	}
	if f.turns.stopped != 1 || len(f.turns.guarded) != 1 || !f.turns.guarded[0] || f.turns.cleared != 0 {
		t.Fatalf("turns %+v: want one forced guard, one StopAll, guard left up", f.turns)
	}
	if read(t, f.binary) != "new 0.2.0" || read(t, f.binary+".previous") != "old" {
		t.Fatal("binary not swapped, or previous not kept")
	}
	select {
	case <-f.restarted:
	case <-time.After(3 * time.Second):
		t.Fatal("host never restarted")
	}
	if _, err := f.u.Update(context.Background(), true); !errors.Is(err, ErrRunning) {
		t.Fatalf("second update while restarting: %v, want ErrRunning", err)
	}
}

func TestFailedDownloadLeavesHostAsIs(t *testing.T) {
	f := newFixture(t, "0.1.0", "0.2.0")
	f.stageErr = errors.New("release checksum mismatch")
	if _, err := f.u.Update(context.Background(), false); !errors.Is(err, f.stageErr) {
		t.Fatalf("err %v", err)
	}
	if read(t, f.binary) != "old" || len(f.turns.guarded) != 0 {
		t.Fatal("a failed download touched the host")
	}
	if _, err := os.Stat(f.marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("marker written for an update that never happened")
	}
	// The lock is released, so the user can try again.
	f.stageErr = nil
	if _, err := f.u.Update(context.Background(), false); err != nil {
		t.Fatal(err)
	}
}

// A chat that starts between the count and the guard refuses the update, and
// the downloaded binary and guard are both cleaned up.
func TestGuardRefusalCleansUp(t *testing.T) {
	f := newFixture(t, "0.1.0", "0.2.0")
	f.turns.guard = errors.New("chat is active")
	if _, err := f.u.Update(context.Background(), false); !errors.Is(err, f.turns.guard) {
		t.Fatalf("err %v", err)
	}
	if read(t, f.binary) != "old" {
		t.Fatal("binary swapped despite the refusal")
	}
	if _, err := os.Stat(filepath.Join(f.dir, ".repogo-update-test")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("staged download left behind")
	}
}

func TestSettledReleaseClearsMarker(t *testing.T) {
	f := newFixture(t, "0.1.0", "0.2.0")
	if _, err := f.u.Update(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if out, err := Resume(f.marker, f.binary, "0.2.0"); err != nil || out != (Outcome{}) {
		t.Fatalf("first start: %+v %v", out, err)
	}
	if err := Settle(f.marker, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.marker); err != nil {
		t.Fatal("settling another version cleared the marker")
	}
	if err := Settle(f.marker, "0.2.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("marker kept after the new release settled")
	}
}

// A release that never reaches Settle is rolled back on its third start, and
// the previous release reports why once it is running again.
func TestReleaseThatCannotStartRollsBack(t *testing.T) {
	f := newFixture(t, "0.1.0", "0.2.0")
	if _, err := f.u.Update(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	for start := 1; start < maxAttempts; start++ {
		out, err := Resume(f.marker, f.binary, "0.2.0")
		if err != nil || out.RolledBack {
			t.Fatalf("start %d: %+v %v", start, out, err)
		}
	}
	out, err := Resume(f.marker, f.binary, "0.2.0")
	if err != nil || !out.RolledBack {
		t.Fatalf("start %d: %+v %v, want a rollback", maxAttempts, out, err)
	}
	if read(t, f.binary) != "old" {
		t.Fatal("binary not rolled back")
	}
	out, err = Resume(f.marker, f.binary, "0.1.0")
	if err != nil || out.Failed != "0.2.0 didn't start, so the host went back to 0.1.0." {
		t.Fatalf("previous release start: %+v %v", out, err)
	}
	if _, err := os.Stat(f.marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("marker kept after the rollback was reported")
	}
}

func TestNoMarkerIsNoUpdate(t *testing.T) {
	dir := t.TempDir()
	out, err := Resume(filepath.Join(dir, "update.json"), filepath.Join(dir, "repogo"), "0.1.0")
	if err != nil || out != (Outcome{}) {
		t.Fatalf("%+v %v", out, err)
	}
}
