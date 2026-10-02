package terminal_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/terminal"
	"github.com/repogo/host/internal/testwait"
)

// roots stands in for the files service: whatever it contains is a project.
type roots struct{ dir string }

func (r roots) Contain(path string) (string, error) { return r.dir, nil }

// collector records what would have gone out on the push lane.
type collector struct {
	mu      sync.Mutex
	out     strings.Builder
	exit    chan int
	changed chan changedPush
}

// changedPush is one tab-set announcement: who heard it, and how many tabs it
// said the project has.
type changedPush struct {
	to    device.ID
	count int
}

func newCollector() *collector {
	return &collector{exit: make(chan int, 1), changed: make(chan changedPush, 8)}
}

// Send decodes what a client would: the events as JSON off the wire.
func (c *collector) Send(to device.ID, method string, payload []byte) error {
	switch method {
	case "terminal.output":
		var ev terminal.Output
		_ = json.Unmarshal(payload, &ev)
		c.mu.Lock()
		c.out.Write(ev.Data)
		c.mu.Unlock()
	case "terminal.exit":
		var ev terminal.Exit
		_ = json.Unmarshal(payload, &ev)
		select {
		case c.exit <- ev.ExitCode:
		default:
		}
	case "terminal.changed":
		var ev terminal.Changed
		_ = json.Unmarshal(payload, &ev)
		select {
		case c.changed <- changedPush{to: to, count: len(ev.Sessions)}:
		default:
		}
	}
	return nil
}

// emitter wraps the collector as what Manager takes.
func (c *collector) emitter() *emit.Emitter { return emit.New(c, quiet()) }

func (c *collector) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.String()
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestSessionRunsCommandsAndReplaysScrollback(t *testing.T) {
	c := newCollector()
	m := terminal.New(roots{t.TempDir()}, c.emitter(), quiet())
	t.Cleanup(m.Shutdown)

	info, err := m.Create("phone", "/anywhere", 80, 24)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.SessionID == "" || info.PID == 0 {
		t.Fatalf("create returned an unusable session: %+v", info)
	}

	if err := m.Input(info.SessionID, []byte("echo repogo-marker\n")); err != nil {
		t.Fatalf("input: %v", err)
	}
	testwait.For(t, "the marker in the output", func() bool { return strings.Contains(c.text(), "repogo-marker") })

	// A second device attaching gets the history it missed — the whole reason
	// the buffer lives on the host.
	buffered, _, err := m.Attach("ipad", info.SessionID)
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	if !strings.Contains(string(buffered), "repogo-marker") {
		t.Fatalf("scrollback lost the session's output: %q", buffered)
	}

	if got := m.List(""); len(got) != 1 || got[0].SessionID != info.SessionID {
		t.Fatalf("list did not report the open session: %+v", got)
	}
}

// The tab strip is the host's, so a shell one device opens has to show up on
// another device that never asked for it.
func TestTabSetIsAnnouncedToEveryWatcher(t *testing.T) {
	dir := t.TempDir()
	c := newCollector()
	m := terminal.New(roots{dir}, c.emitter(), quiet())
	t.Cleanup(m.Shutdown)

	if err := m.Subscribe("ipad", dir); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	info, err := m.Create("phone", dir, 80, 24)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := waitChanged(t, c); got.to != "ipad" || got.count != 1 {
		t.Fatalf("the watcher was told %+v, want one tab for ipad", got)
	}

	if err := m.Close(info.SessionID); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := waitChanged(t, c); got.count != 0 {
		t.Fatalf("closing left the watcher on %d tabs", got.count)
	}
}

func waitChanged(t *testing.T, c *collector) changedPush {
	t.Helper()
	select {
	case got := <-c.changed:
		return got
	case <-time.After(10 * time.Second):
		t.Fatal("no terminal.changed push")
		return changedPush{}
	}
}

func TestCloseEndsTheSessionAndAnnouncesIt(t *testing.T) {
	c := newCollector()
	m := terminal.New(roots{t.TempDir()}, c.emitter(), quiet())
	t.Cleanup(m.Shutdown)

	info, err := m.Create("phone", "/anywhere", 80, 24)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := m.Close(info.SessionID); err != nil {
		t.Fatalf("close: %v", err)
	}

	select {
	case <-c.exit:
	case <-time.After(10 * time.Second):
		t.Fatal("no terminal.exit push after close")
	}
	if err := m.Input(info.SessionID, []byte("x")); err == nil {
		t.Fatal("a closed session still accepted input")
	}
}
