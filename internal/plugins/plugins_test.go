package plugins

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/errkind"
)

const notesPlugin = `import { definePlugin, method, z } from "@repogo/plugins";
export default definePlugin({
  collections: { notes: z.object({ text: z.string() }) },
  methods: {
    texts: method(z.object({}), async (_, ctx) => (await ctx.db.notes.list({ order: "asc" })).map((n) => n.text), { kind: "query" }),
  },
});
`

// pushes collects what the service sends devices.
type pushes struct {
	mu   sync.Mutex
	sent []sent
	more chan struct{}
}

type sent struct {
	to   device.ID
	data Data
}

func (p *pushes) emit(d device.ID, ev emit.Event) error {
	p.mu.Lock()
	p.sent = append(p.sent, sent{d, ev.(Data)})
	p.mu.Unlock()
	select {
	case p.more <- struct{}{}:
	default:
	}
	return nil
}

// next waits for a push matching the test, or fails after a few seconds.
func (p *pushes) next(t *testing.T, seen int, match func(sent) bool) sent {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		p.mu.Lock()
		for _, s := range p.sent[seen:] {
			if match(s) {
				p.mu.Unlock()
				return s
			}
		}
		p.mu.Unlock()
		select {
		case <-p.more:
		case <-deadline:
			t.Fatal("no matching push")
		}
	}
}

func (p *pushes) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.sent)
}

// sdk is this repo's plugins SDK, with its dependencies installed; tests that run
// plugins skip without it or without Bun.
func sdk(t *testing.T) string {
	t.Helper()
	if _, err := bunPath(); err != nil {
		t.Skip("bun is not installed")
	}
	dir, err := filepath.Abs("../../plugins-sdk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules", "zod")); err != nil {
		t.Skip("plugins-sdk has no node_modules; run bun install there")
	}
	return dir
}

// project is a folder with the SDK linked into .repogo/plugins and these plugins written.
func project(t *testing.T, sdkDir string, plugins map[string]string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".repogo", "plugins")
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "@repogo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if sdkDir != "" {
		if err := os.Symlink(sdkDir, filepath.Join(dir, "node_modules", "@repogo", "plugins")); err != nil {
			t.Fatal(err)
		}
	}
	for name, source := range plugins {
		writePlugin(t, root, name, source)
	}
	return root
}

func writePlugin(t *testing.T, root, name, source string) {
	t.Helper()
	dir := filepath.Join(root, ".repogo", "plugins", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.ts"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newService(t *testing.T, sdkDir string) (*Service, *pushes) {
	t.Helper()
	// A Unix socket's path is capped near 100 bytes; t.TempDir's can be longer.
	home, err := os.MkdirTemp("", "rga")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	p := &pushes{more: make(chan struct{}, 1)}
	s := New(t.Context(), Config{
		Runtime: sdkRuntime(sdkDir), Socket: filepath.Join(home, "plugins.sock"), DataDir: filepath.Join(home, "data"),
		Emit: p.emit, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return s, p
}

func texts(t *testing.T, raw json.RawMessage) []string {
	t.Helper()
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return out
}

// sdkRuntime is the SDK's source entry, or "" for a host without a runtime.
func sdkRuntime(sdkDir string) string {
	if sdkDir == "" {
		return ""
	}
	return filepath.Join(sdkDir, "src", "run.ts")
}

func TestWithoutARuntimePluginsAreOff(t *testing.T) {
	s, _ := newService(t, "")
	root := project(t, "", map[string]string{"notes": notesPlugin})
	if s.On() || s.Has(root) {
		t.Fatal("a host without a runtime offers plugins")
	}
	if _, err := s.Call(t.Context(), root, "notes.texts", nil, true); !errors.Is(err, ErrOff) {
		t.Fatalf("err = %v, want ErrOff", err)
	}
}

func TestAProjectWithoutPluginsIsNotFound(t *testing.T) {
	s, _ := newService(t, "/sdk")
	if _, err := s.Call(t.Context(), t.TempDir(), "notes.texts", nil, true); !errors.Is(err, ErrNoPlugins) {
		t.Fatalf("err = %v, want ErrNoPlugins", err)
	}
	if _, err := s.Call(t.Context(), "relative/path", "notes.texts", nil, true); err == nil {
		t.Fatal("a relative project was accepted")
	}
}

func TestCallAndListRunTheProjectsPlugins(t *testing.T) {
	dir := sdk(t)
	s, _ := newService(t, dir)
	root := project(t, dir, map[string]string{"notes": notesPlugin})

	list, err := s.List(t.Context(), root)
	if err != nil || list.Error != nil || !strings.Contains(string(list.Result), `"notes.texts"`) {
		t.Fatalf("list = %s %+v %v", list.Result, list.Error, err)
	}
	if _, err := s.Call(t.Context(), root, "notes.notes.create", json.RawMessage(`{"text":"a"}`), true); err != nil {
		t.Fatal(err)
	}
	got, err := s.Call(t.Context(), root, "notes.texts", nil, true)
	if err != nil || strings.Join(texts(t, got.Result), ",") != "a" {
		t.Fatalf("texts = %s %v", got.Result, err)
	}
	bad, err := s.Call(t.Context(), root, "notes.notes.create", json.RawMessage(`{}`), true)
	if err != nil || bad.Error == nil || bad.Error.Code != "VALIDATION" || bad.Error.FieldErrors["/text"] == "" {
		t.Fatalf("bad input = %+v %v, want the plugin's field error", bad.Error, err)
	}
}

// A subscription's device gets plugins.data when a write changes the answer, and
// another device can't end it.
func TestASubscriptionPushesToItsDevice(t *testing.T) {
	dir := sdk(t)
	s, p := newService(t, dir)
	root := project(t, dir, map[string]string{"notes": notesPlugin})

	sub, err := s.Subscribe(t.Context(), "phone", root, "notes.texts", "s1", nil)
	if err != nil || sub.Subscription == "" || sub.Revision != 1 || len(texts(t, sub.Result)) != 0 {
		t.Fatalf("subscribe = %+v %v", sub, err)
	}
	if _, err := s.Call(t.Context(), root, "notes.notes.create", json.RawMessage(`{"text":"a"}`), true); err != nil {
		t.Fatal(err)
	}
	got := p.next(t, 0, func(s sent) bool { return s.data.Subscription == sub.Subscription })
	if got.to != "phone" || got.data.Revision != 2 || strings.Join(texts(t, got.data.Result), ",") != "a" {
		t.Fatalf("push = %+v", got)
	}
	if err := s.Unsubscribe(t.Context(), "ipad", sub.Subscription); !errors.Is(err, ErrNotYours) {
		t.Fatalf("another device's unsubscribe: %v", err)
	}
}

// A device that leaves takes its subscriptions with it.
func TestLeavingEndsADevicesSubscriptions(t *testing.T) {
	dir := sdk(t)
	s, p := newService(t, dir)
	root := project(t, dir, map[string]string{"notes": notesPlugin})
	if _, err := s.Subscribe(t.Context(), "phone", root, "notes.texts", "s2", nil); err != nil {
		t.Fatal(err)
	}
	stays, err := s.Subscribe(t.Context(), "ipad", root, "notes.texts", "s3", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Leave("phone")
	if _, err := s.Call(t.Context(), root, "notes.notes.create", json.RawMessage(`{"text":"a"}`), true); err != nil {
		t.Fatal(err)
	}
	p.next(t, 0, func(s sent) bool { return s.data.Subscription == stays.Subscription })
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sent {
		if s.to == "phone" {
			t.Fatalf("the phone that left got %+v", s)
		}
	}
}

// An agent edits a plugin and calls it straight away: the call runs the new code,
// and the open subscription gets what the new code answers.
func TestEditedPluginsReloadBeforeTheNextCall(t *testing.T) {
	dir := sdk(t)
	s, p := newService(t, dir)
	root := project(t, dir, map[string]string{"notes": notesPlugin})
	if _, err := s.Call(t.Context(), root, "notes.notes.create", json.RawMessage(`{"text":"a"}`), true); err != nil {
		t.Fatal(err)
	}
	sub, err := s.Subscribe(t.Context(), "phone", root, "notes.texts", "s4", nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := p.count()

	writePlugin(t, root, "notes", strings.Replace(notesPlugin, "(n) => n.text", "(n) => n.text.toUpperCase()", 1))
	// A later mtime: some file systems keep a second's resolution.
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(filepath.Join(root, ".repogo", "plugins", "notes", "app.ts"), future, future)

	got, err := s.Call(t.Context(), root, "notes.texts", nil, true)
	if err != nil || strings.Join(texts(t, got.Result), ",") != "A" {
		t.Fatalf("after the edit = %s %v", got.Result, err)
	}
	pushed := p.next(t, seen, func(s sent) bool { return s.data.Subscription == sub.Subscription })
	if strings.Join(texts(t, pushed.data.Result), ",") != "A" {
		t.Fatalf("push after reload = %s", pushed.data.Result)
	}
}

// The server going away leaves the subscriptions standing: a new server takes
// them up under the same ids and the device gets the answer again.
func TestALostServerTakesItsSubscriptionsBackUp(t *testing.T) {
	dir := sdk(t)
	s, p := newService(t, dir)
	root := project(t, dir, map[string]string{"notes": notesPlugin})
	sub, err := s.Subscribe(t.Context(), "phone", root, "notes.texts", "s5", nil)
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	old := s.server
	s.mu.Unlock()
	old.close()

	got := p.next(t, 0, func(s sent) bool { return s.data.Subscription == sub.Subscription })
	if got.data.Revision != 2 || got.data.Error != nil {
		t.Fatalf("push after restart = %+v", got.data)
	}
	if _, err := s.Call(t.Context(), root, "notes.notes.create", json.RawMessage(`{"text":"b"}`), true); err != nil {
		t.Fatal(err)
	}
	again := p.next(t, 1, func(s sent) bool { return s.data.Subscription == sub.Subscription })
	if strings.Join(texts(t, again.data.Result), ",") != "b" {
		t.Fatalf("push on the new server = %+v", again.data)
	}
}

// Each device names its own subscriptions: the same id on two devices is two
// subscriptions, and a bad id is refused.
func TestSubscriptionIDsBelongToTheirDevice(t *testing.T) {
	dir := sdk(t)
	s, p := newService(t, dir)
	root := project(t, dir, map[string]string{"notes": notesPlugin})
	if _, err := s.Subscribe(t.Context(), "phone", root, "notes.texts", "list", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Subscribe(t.Context(), "ipad", root, "notes.texts", "list", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Subscribe(t.Context(), "phone", root, "notes.texts", "bad id/", nil); !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := s.Call(t.Context(), root, "notes.notes.create", json.RawMessage(`{"text":"a"}`), true); err != nil {
		t.Fatal(err)
	}
	p.next(t, 0, func(s sent) bool { return s.to == "phone" && s.data.Subscription == "list" })
	p.next(t, 0, func(s sent) bool { return s.to == "ipad" && s.data.Subscription == "list" })
}

// Subscribing again under an id replaces it: the next write pushes once.
func TestSubscribingAgainReplaces(t *testing.T) {
	dir := sdk(t)
	s, p := newService(t, dir)
	root := project(t, dir, map[string]string{"notes": notesPlugin})
	for range 2 {
		if _, err := s.Subscribe(t.Context(), "phone", root, "notes.texts", "list", nil); err != nil {
			t.Fatal(err)
		}
	}
	s.mu.Lock()
	held := len(s.subs)
	s.mu.Unlock()
	if held != 1 {
		t.Fatalf("subscriptions held = %d, want 1", held)
	}
	if _, err := s.Call(t.Context(), root, "notes.notes.create", json.RawMessage(`{"text":"a"}`), true); err != nil {
		t.Fatal(err)
	}
	p.next(t, 0, func(s sent) bool { return s.data.Subscription == "list" })
	if _, err := s.Call(t.Context(), root, "notes.notes.create", json.RawMessage(`{"text":"b"}`), true); err != nil {
		t.Fatal(err)
	}
	got := p.next(t, 1, func(s sent) bool { return s.data.Subscription == "list" })
	if got.data.Revision != 3 || p.count() != 2 {
		t.Fatalf("pushes = %d, last revision %d; want 2 pushes, revision 3", p.count(), got.data.Revision)
	}
}
