package chatlive

import (
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"

	"github.com/repogoapp/host/internal/chatwire"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/session"
	"github.com/repogoapp/host/internal/store"
	"github.com/repogoapp/host/internal/testwait"
)

const launchAnswer = "Async agent launched successfully."

// A background agent's report rewrites a row in place and appends nothing.
// An open sheet still gets it, and the devices watching keep their history:
// no page of a new generation is sent.
func TestALateResultReachesAnOpenSheetWithoutAReload(t *testing.T) {
	home, db, m, pub := seeded(t, userLine("audit it"), toolUseLine("toolu_1", "audit"), toolResultLine("toolu_1", launchAnswer))
	db.SameRow(chatwire.New(nil).SameRow)
	live, err := m.Open(phone, "claude:live-1", nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Unsubscribe(phone)
	first, err := m.WatchTools(phone, "claude:live-1", []string{"toolu_1"})
	if err != nil || len(first) != 1 || first[0].Output != launchAnswer {
		t.Fatalf("the sheet opened on %+v (%v), want the launch's answer", first, err)
	}

	writeClaudeSession(t, home, "live-1", []string{toolResultLine("toolu_1", "Found two bugs.")})
	testwait.For(t, "the late result on the sheet", func() bool {
		tools := pub.receivedTools()
		return len(tools) > 0 && tools[len(tools)-1].Output == "Found two bugs."
	})
	for _, page := range pub.receivedBy(phone) {
		if page.Generation != live.Page.Generation {
			t.Errorf("a page of generation %d reached the device, want %d kept", page.Generation, live.Page.Generation)
		}
	}
}

// lateHome is a chat whose Agent call has only its launch answer, synced into
// a store, and how to sync it again after the file grows.
func lateHome(t *testing.T) (home string, sessions *session.Store, db *store.Store, sync func()) {
	t.Helper()
	home, sessions = liveHome(t, userLine("audit it"), toolUseLine("toolu_1", "audit"), toolResultLine("toolu_1", launchAnswer))
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SameRow(chatwire.New(nil).SameRow)
	sync = func() {
		t.Helper()
		metas, _ := sessions.List()
		events, err := sessions.Events(metas[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Sync(metas[0], events); err != nil {
			t.Fatal(err)
		}
	}
	sync()
	return home, sessions, db, sync
}

func outputs(tools []Tool) []string {
	out := make([]string, len(tools))
	for i, tool := range tools {
		out[i] = tool.Output
	}
	return out
}

// The sweep can import the late result before the chat's own flush, which
// then finds the file unchanged. The flush compares what it last sent each
// sheet with the store, so the sheet hears it all the same, once.
func TestAResultTheSweepImportedStillReachesTheSheet(t *testing.T) {
	home, sessions, db, sync := lateHome(t)
	pub := &capture{}
	m := newManager(t, sessions, db, pub, &fakeTurns{})
	if _, err := m.WatchTools(phone, "claude:live-1", []string{"toolu_1"}); err != nil {
		t.Fatal(err)
	}
	flush := func() {
		t.Helper()
		if err := m.pushTools(phone, "claude:live-1"); err != nil {
			t.Fatal(err)
		}
	}

	// A sheet is sent its call once after it opens; unchanged, nothing more.
	flush()
	flush()
	if got := outputs(pub.receivedTools()); len(got) != 1 || got[0] != launchAnswer {
		t.Fatalf("pushed %q, want the call once as the sheet opened", got)
	}

	writeClaudeSession(t, home, "live-1", []string{toolResultLine("toolu_1", "Found two bugs.")})
	sync()
	flush()
	flush()
	if got := outputs(pub.receivedTools()); len(got) != 2 || got[1] != "Found two bugs." {
		t.Fatalf("pushed %q, want the late result once more", got)
	}

	// A closed sheet hears nothing more.
	m.UnwatchTools(phone, "claude:live-1", []string{"toolu_1"})
	writeClaudeSession(t, home, "live-1", []string{toolResultLine("toolu_1", "Found three.")})
	sync()
	flush()
	if got := pub.receivedTools(); len(got) != 2 {
		t.Fatalf("a closed sheet was pushed to: %+v", got)
	}
}

// heldTransport holds each chats.tool send until released, and can refuse them.
type heldTransport struct {
	mu      sync.Mutex
	tools   []Tool
	hold    chan struct{}
	holding chan struct{}
	fail    bool
}

func (h *heldTransport) Send(_ device.ID, method string, payload []byte) error {
	if method != "chats.tool" {
		return nil
	}
	h.mu.Lock()
	hold, holding, fail := h.hold, h.holding, h.fail
	h.mu.Unlock()
	if fail {
		return errors.New("the device is gone")
	}
	if hold != nil {
		close(holding)
		<-hold
	}
	var tool Tool
	if err := json.Unmarshal(payload, &tool); err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tools = append(h.tools, tool)
	return nil
}

func (h *heldTransport) received() []Tool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Tool(nil), h.tools...)
}

func heldManager(t *testing.T, sessions *session.Store, db *store.Store, pub emit.Transport) *Manager {
	return New(t.Context(), Deps{
		Sessions: sessions, Store: db, Emit: emit.New(pub, func() []device.ID { return nil }, slog.Default()),
		Turns: &fakeTurns{}, Log: slog.Default(), Attention: func(Attention) {},
		Wire: chatwire.New(nil), Children: noChildren{},
	})
}

// A flush that read the store before the late result can deliver its older
// detail after a sheet's subscribe answered with the newer one, and a device
// applies pushes after the answer. The flush the subscribe asks for sends the
// call again, so the sheet ends on what the store holds.
func TestAnOlderPushAfterTheAnswerIsCorrected(t *testing.T) {
	home, sessions, db, sync := lateHome(t)
	pub := &heldTransport{}
	m := heldManager(t, sessions, db, pub)
	if _, err := m.WatchTools(phone, "claude:live-1", []string{"toolu_1"}); err != nil {
		t.Fatal(err)
	}

	// A flush reads the launch answer and is held mid-send.
	pub.hold, pub.holding = make(chan struct{}), make(chan struct{})
	flushed := make(chan error, 1)
	go func() { flushed <- m.pushTools(phone, "claude:live-1") }()
	<-pub.holding

	// The report lands, and the sheet subscribes again: its answer is the report.
	writeClaudeSession(t, home, "live-1", []string{toolResultLine("toolu_1", "Found two bugs.")})
	sync()
	answer, err := m.WatchTools(phone, "claude:live-1", []string{"toolu_1"})
	if err != nil || len(answer) != 1 || answer[0].Output != "Found two bugs." {
		t.Fatalf("answer = %+v (%v), want the report", answer, err)
	}

	// The held send goes out after that answer: the older detail, last.
	pub.mu.Lock()
	hold := pub.hold
	pub.hold, pub.holding = nil, nil
	pub.mu.Unlock()
	close(hold)
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
	if got := outputs(pub.received()); len(got) != 1 || got[0] != launchAnswer {
		t.Fatalf("the held flush sent %q, want the launch answer", got)
	}

	// The flush the subscribe asked for corrects it, once.
	for range 2 {
		if err := m.pushTools(phone, "claude:live-1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := outputs(pub.received()); len(got) != 2 || got[1] != "Found two bugs." {
		t.Fatalf("the sheet was left on %q, want the report sent after the older push", got)
	}
}

// A send that fails is not what the device has: the next flush sends it again.
func TestAFailedToolPushIsSentAgain(t *testing.T) {
	_, sessions, db, _ := lateHome(t)
	pub := &heldTransport{fail: true}
	m := heldManager(t, sessions, db, pub)
	if _, err := m.WatchTools(phone, "claude:live-1", []string{"toolu_1"}); err != nil {
		t.Fatal(err)
	}
	if err := m.pushTools(phone, "claude:live-1"); err == nil {
		t.Fatal("a refused send was reported as sent")
	}

	pub.mu.Lock()
	pub.fail = false
	pub.mu.Unlock()
	for range 2 {
		if err := m.pushTools(phone, "claude:live-1"); err != nil {
			t.Fatal(err)
		}
	}
	if got := outputs(pub.received()); len(got) != 1 || got[0] != launchAnswer {
		t.Fatalf("after the device came back it got %q, want the call once", got)
	}
}

// The watch starts before the read, so a result that landed before the
// subscribe is in its answer.
func TestToolsSubscribeAnswersWhatLandedBeforeIt(t *testing.T) {
	home, sessions, db, sync := lateHome(t)
	writeClaudeSession(t, home, "live-1", []string{toolResultLine("toolu_1", "Found two bugs.")})
	sync()
	m := newManager(t, sessions, db, &capture{}, &fakeTurns{})
	answer, err := m.WatchTools(phone, "claude:live-1", []string{"toolu_1"})
	if err != nil || len(answer) != 1 || answer[0].Output != "Found two bugs." || answer[0].State != chatwire.StateCompleted {
		t.Fatalf("answer = %+v (%v), want the result that had landed", answer, err)
	}
}
