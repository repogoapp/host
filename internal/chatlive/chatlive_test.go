package chatlive

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/chatwire"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
	"github.com/repogo/host/internal/testwait"
)

// A recording publisher, so a test can assert what a device would have received.
type capture struct {
	mu    sync.Mutex
	pages []chatwire.Page
	tools []Tool
}

func (c *capture) Send(_ device.ID, method string, payload []byte) error {
	if method == "chats.tool" {
		var tool Tool
		if err := json.Unmarshal(payload, &tool); err != nil {
			return err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.tools = append(c.tools, tool)
		return nil
	}
	if method != "chats.appended" {
		return nil
	}
	var page chatwire.Page
	if err := json.Unmarshal(payload, &page); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pages = append(c.pages, page)
	return nil
}

func (c *capture) received() []chatwire.Page {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]chatwire.Page(nil), c.pages...)
}

func (c *capture) receivedTools() []Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Tool(nil), c.tools...)
}

// newManager serves sessions and db to pub, every hook a no-op.
func newManager(t *testing.T, sessions *session.Store, db *store.Store, pub emit.Transport, turns TurnStream) *Manager {
	return New(t.Context(), Deps{
		Sessions: sessions, Store: db, Emit: emit.New(pub, slog.Default()), Turns: turns, Log: slog.Default(),
		HostID: "host-1", Attention: func(Attention) {},
		Approval: func(string, string, *agent.Approval) {}, Wire: chatwire.New(nil),
	})
}

// liveHome is a temporary Claude home holding one session, live-1, and the
// store that reads it.
func liveHome(t *testing.T, lines ...string) (string, *session.Store) {
	t.Helper()
	home := t.TempDir()
	writeClaudeSession(t, home, "live-1", lines)
	return home, session.NewStore(claude.NewSessions(filepath.Join(home, ".claude")))
}

// writeClaudeSession appends JSONL lines to a Claude-shaped session file.
func writeClaudeSession(t *testing.T, home, sessionID string, lines []string) string {
	t.Helper()
	dir := filepath.Join(home, ".claude", "projects", "-srv-demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// userLine is one prompt in a project outside the temp roots, which session
// discovery leaves out.
func userLine(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type":      "user",
		"cwd":       "/srv/demo",
		"sessionId": "live-1",
		"message":   map[string]any{"role": "user", "content": text},
	})
	return string(b)
}

// The whole point of the package: a file that grows while a device is watching
// produces a push, without the device asking again.
func TestAppendProducesAPush(t *testing.T) {
	home, sessions := liveHome(t, userLine("first"))
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	// Seed the cache the way the periodic sweep would.
	metas, _ := sessions.List()
	if len(metas) == 0 {
		t.Fatal("session discovery found nothing in the temporary home")
	}
	events, err := sessions.Events(metas[0])
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	if err := db.Sync(metas[0], events); err != nil {
		t.Fatalf("seed: %v", err)
	}

	seeded, err := db.Messages("claude:live-1", 0, 100)
	if err != nil {
		t.Fatalf("seed read: %v", err)
	}

	pub := &capture{}
	m := newManager(t, sessions, db, pub, &fakeTurns{})

	caller := device.ID("00112233445566778899aabbccddeeff")
	if _, err := m.Subscribe(caller, "claude:live-1", seeded.NextIdx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer m.Unsubscribe(caller)

	// The user keeps talking in their own terminal.
	writeClaudeSession(t, home, "live-1", []string{userLine("second")})

	testwait.For(t, "a push", func() bool { return len(pub.received()) > 0 })

	page := pub.received()[0]
	if len(page.Events) == 0 {
		t.Fatal("pushed a page with no events")
	}
	// Only the tail: the client already had everything up to the cursor, and
	// resending it would duplicate rows on screen.
	if page.Events[0].Idx < seeded.NextIdx {
		t.Errorf("push resent event %d, client was already at %d",
			page.Events[0].Idx, seeded.NextIdx)
	}
	if page.NextIdx <= seeded.NextIdx {
		t.Errorf("cursor did not advance: %d → %d", seeded.NextIdx, page.NextIdx)
	}
}

func TestUnsubscribeStopsPushes(t *testing.T) {
	home, sessions := liveHome(t, userLine("first"))
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	metas, _ := sessions.List()
	if len(metas) == 0 {
		t.Fatal("session discovery found nothing in the temporary home")
	}
	events, _ := sessions.Events(metas[0])
	if err := db.Sync(metas[0], events); err != nil {
		t.Fatal(err)
	}

	pub := &capture{}
	m := newManager(t, sessions, db, pub, &fakeTurns{})
	logs := &messages{seen: map[string]bool{}}
	m.log = slog.New(logs)

	caller := device.ID("00112233445566778899aabbccddeeff")
	if _, err := m.Subscribe(caller, "claude:live-1", 0); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	testwait.For(t, "the tail to start", func() bool { return logs.has("chatlive: watching") })
	m.Unsubscribe(caller)
	// A tailer that outlives its subscriber keeps a file handle and a goroutine
	// per chat the user ever opened.
	testwait.For(t, "the tail to stop", func() bool { return logs.has("chatlive: stopped watching") })

	pushed := len(pub.received())
	writeClaudeSession(t, home, "live-1", []string{userLine("after unsubscribe")})
	m.Nudge("claude:live-1")
	if got := len(pub.received()); got != pushed {
		t.Errorf("received %d pushes after unsubscribing", got-pushed)
	}
}

// messages is a slog handler remembering which messages were logged.
type messages struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (h *messages) Enabled(context.Context, slog.Level) bool { return true }
func (h *messages) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *messages) WithGroup(string) slog.Handler            { return h }
func (h *messages) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seen[r.Message] = true
	return nil
}

func (h *messages) has(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seen[msg]
}

func TestSubscribeRejectsUnknownChat(t *testing.T) {
	sessions := session.NewStore(claude.NewSessions(filepath.Join(t.TempDir(), ".claude")))
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	m := newManager(t, sessions, db, &capture{}, &fakeTurns{})
	if _, err := m.Subscribe("00112233445566778899aabbccddeeff", "claude:nope", 0); err == nil {
		t.Error("subscribed to a chat that does not exist")
	}
}

// A hook saying the transcript changed flushes the tail now, without waiting
// for the file poll.
func TestNudgeFlushesWatchers(t *testing.T) {
	home, sessions := liveHome(t, userLine("first"))
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	metas, _ := sessions.List()
	if len(metas) == 0 {
		t.Fatal("session discovery found nothing in the temporary home")
	}
	events, err := sessions.Events(metas[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Sync(metas[0], events); err != nil {
		t.Fatal(err)
	}
	seeded, err := db.Messages("claude:live-1", 0, 100)
	if err != nil {
		t.Fatal(err)
	}

	pub := &capture{}
	m := newManager(t, sessions, db, pub, &fakeTurns{})
	caller := device.ID("00112233445566778899aabbccddeeff")
	if _, err := m.Subscribe(caller, "claude:live-1", seeded.NextIdx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer m.Unsubscribe(caller)

	writeClaudeSession(t, home, "live-1", []string{userLine("second")})
	m.Nudge("claude:live-1")
	testwait.For(t, "a nudged push", func() bool { return len(pub.received()) > 0 })
	if page := pub.received()[0]; page.NextIdx <= seeded.NextIdx {
		t.Errorf("cursor did not advance: %d → %d", seeded.NextIdx, page.NextIdx)
	}
}

// A backlog larger than one page reaches the device in one flush: nothing
// else may write to the transcript to trigger the next.
func TestFlushSendsEveryPageOfABacklog(t *testing.T) {
	lines := make([]string, flushPage+120)
	for i := range lines {
		lines[i] = userLine("line")
	}
	_, sessions := liveHome(t, lines...)
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if metas, _ := sessions.List(); len(metas) == 0 {
		t.Fatal("session discovery found nothing in the temporary home")
	}

	pub := &capture{}
	m := newManager(t, sessions, db, pub, &fakeTurns{})
	caller := device.ID("00112233445566778899aabbccddeeff")
	next, err := m.flush(context.Background(), caller, "claude:live-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	pages := pub.received()
	if len(pages) < 2 {
		t.Fatalf("pushed %d pages, want the backlog split across several", len(pages))
	}
	last := pages[len(pages)-1]
	if next != last.EventCount || last.NextIdx != last.EventCount {
		t.Fatalf("stopped at %d of %d", next, last.EventCount)
	}
	for i := 1; i < len(pages); i++ {
		if pages[i].Events[0].Idx != pages[i-1].NextIdx {
			t.Fatalf("page %d starts at %d, previous ended at %d", i, pages[i].Events[0].Idx, pages[i-1].NextIdx)
		}
	}
}

// toolUseLine is Claude calling a tool; toolResultLine is what came back.
func toolUseLine(id, command string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant", "cwd": "/srv/demo", "sessionId": "live-1",
		"message": map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": command}},
		}},
	})
	return string(b)
}

func toolResultLine(id, output string) string {
	b, _ := json.Marshal(map[string]any{
		"type": "user", "cwd": "/srv/demo", "sessionId": "live-1",
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": id, "content": output},
		}},
	})
	return string(b)
}

// An open tool sheet gets its call whole as the result lands; a call it
// doesn't show sends nothing, and a closed sheet stops the pushes.
func TestWatchedToolsArePushedWhole(t *testing.T) {
	home, sessions := liveHome(t, userLine("list it"), toolUseLine("toolu_1", "ls"), toolUseLine("toolu_2", "pwd"))
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	metas, _ := sessions.List()
	if len(metas) == 0 {
		t.Fatal("session discovery found nothing in the temporary home")
	}
	events, _ := sessions.Events(metas[0])
	if err := db.Sync(metas[0], events); err != nil {
		t.Fatal(err)
	}
	seeded, _ := db.Messages("claude:live-1", 0, 100)

	pub := &capture{}
	m := newManager(t, sessions, db, pub, &fakeTurns{})
	caller := device.ID("00112233445566778899aabbccddeeff")
	// Watched before the chat is subscribed, as after a reconnect.
	m.WatchTools(caller, "claude:live-1", []string{"toolu_1"})
	if _, err := m.Subscribe(caller, "claude:live-1", seeded.NextIdx); err != nil {
		t.Fatal(err)
	}
	defer m.Unsubscribe(caller)

	writeClaudeSession(t, home, "live-1", []string{toolResultLine("toolu_2", "/srv"), toolResultLine("toolu_1", "a.txt")})
	testwait.For(t, "the watched call", func() bool { return len(pub.receivedTools()) > 0 })

	tools := pub.receivedTools()
	if len(tools) != 1 {
		t.Fatalf("pushed %d calls, want only the watched one: %+v", len(tools), tools)
	}
	got := tools[0]
	if got.ChatID != "claude:live-1" || got.CallID != "toolu_1" || got.Name != "Bash" ||
		got.State != chatwire.StateCompleted || got.Output != "a.txt" || !strings.Contains(string(got.Input), `"ls"`) {
		t.Errorf("pushed %+v", got)
	}

	m.UnwatchTools(caller, "claude:live-1", []string{"toolu_1"})
	pushed := len(pub.received())
	writeClaudeSession(t, home, "live-1", []string{toolUseLine("toolu_1", "ls again")})
	m.Nudge("claude:live-1")
	testwait.For(t, "the next page", func() bool { return len(pub.received()) > pushed })
	if n := len(pub.receivedTools()); n != 1 {
		t.Errorf("a closed sheet still received %d more pushes", n-1)
	}
}
