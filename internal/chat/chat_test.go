package chat_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/chat"
	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/store"
	"github.com/repogo/host/internal/testhost"
)

// recorder is a Sender that remembers what the host would have run.
type recorder struct {
	last agent.TurnRequest
	n    int
	live []agent.TurnStatus
}

func (r *recorder) List() []agent.TurnStatus { return r.live }

func (r *recorder) Send(req agent.TurnRequest) (agent.TurnStatus, error) {
	r.last = req
	r.n++
	return agent.TurnStatus{TurnID: "turn-1", State: agent.StateQueued}, nil
}

func (r *recorder) Steer(req agent.TurnRequest) (agent.TurnStatus, bool, error) {
	status, err := r.Send(req)
	return status, false, err
}

func (r *recorder) StopChat(context.Context, string) error { return agent.ErrNotFound }

// transcripts is a Transcripts that remembers deletions and serves one subagent.
type transcripts struct {
	deleted         []string
	renamed         []string
	sessionID, call string
}

func (f *transcripts) Delete(sessionID string) error {
	f.deleted = append(f.deleted, sessionID)
	return nil
}

func (f *transcripts) Rename(sessionID, title string) error {
	f.renamed = append(f.renamed, sessionID+": "+title)
	return nil
}

func (f *transcripts) Subagent(sessionID, callID string) (session.Subagent, error) {
	f.sessionID, f.call = sessionID, callID
	if callID != "toolu_task" {
		return session.Subagent{}, session.ErrNotFound
	}
	return session.Subagent{Kind: "Explore", Events: []agent.Event{{Kind: agent.EventText, Text: "It is in claude.go."}}}, nil
}

// harness is the chat service with the Deps it was built from.
type harness struct {
	*chat.Service
	chat.Deps
}

// newService is the testhost's chat service with a recording Sender and
// Transcripts, and one chat, kind:sess-1 in /tmp/project; opts adjust the rest.
func newService(t *testing.T, kind agent.Kind, opts ...func(*chat.Deps)) (*harness, *recorder, *transcripts) {
	t.Helper()
	d := testhost.New(t).ChatDeps
	rec, files := &recorder{}, &transcripts{}
	d.Sender, d.Transcripts = rec, files
	for _, opt := range opts {
		opt(&d)
	}
	svc, err := chat.New(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Cache.Sync(session.Meta{
		Agent: kind, ID: "sess-1", Title: "demo", Cwd: "/tmp/project", UpdatedAt: time.UnixMilli(1000), SizeBytes: 1,
	}, []agent.Event{{Kind: agent.EventText, Text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	return &harness{svc, d}, rec, files
}

// A chat service missing a dependency fails at construction, naming it, not on first use.
func TestNewNamesMissingDeps(t *testing.T) {
	d := testhost.New(t).ChatDeps
	d.Cache, d.Files = nil, nil
	if _, err := chat.New(d); err == nil || !strings.Contains(err.Error(), "Cache, Files") {
		t.Fatalf("New = %v, want Cache and Files named", err)
	}
}

// Sending must continue the provider's conversation where the chat lives.
func TestSendResumesTheChatInItsOwnFolder(t *testing.T) {
	svc, rec, _ := newService(t, agent.KindClaude)
	status, err := svc.Send("claude:sess-1", chat.Turn{Prompt: "run the tests"})
	if err != nil {
		t.Fatal(err)
	}
	if status.TurnID != "turn-1" {
		t.Errorf("turn = %q", status.TurnID)
	}
	// Forking the session would orphan the transcript on screen, and a folder
	// from the caller could run an agent anywhere on the machine.
	if rec.last.SessionID != "sess-1" || rec.last.Cwd != "/tmp/project" || rec.last.Agent != agent.KindClaude || rec.last.Prompt != "run the tests" {
		t.Errorf("queued %+v", rec.last)
	}
}

func TestSendRefusesAnEmptyPromptBeforeQueueing(t *testing.T) {
	svc, rec, _ := newService(t, agent.KindClaude)
	if _, err := svc.Send("claude:sess-1", chat.Turn{Prompt: "  "}); !(errkind.Of(err) == errkind.Invalid) {
		t.Errorf("empty prompt: %v, want invalid", err)
	}
	if _, err := svc.Send("claude:nope", chat.Turn{Prompt: "hello"}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown chat: %v, want not found", err)
	}
	if rec.n != 0 {
		t.Errorf("%d turns queued", rec.n)
	}
}

// Resolving moves a chat to the done pile, and sending reopens it: nobody
// types "unresolve" first.
func TestResolveThenSendReopens(t *testing.T) {
	svc, _, _ := newService(t, agent.KindClaude)
	resolved := true
	live := func() int {
		open := false
		page, err := svc.List(t.Context(), store.ChatQuery{Resolved: &open}, false)
		if err != nil {
			t.Fatal(err)
		}
		return len(page.Chats)
	}
	c, err := svc.Resolve("claude:sess-1", resolved)
	if err != nil || c.ResolvedAt == nil {
		t.Fatalf("resolve = %+v, %v", c, err)
	}
	if live() != 0 {
		t.Fatal("resolved chat still in the live list")
	}
	if _, err := svc.Send("claude:sess-1", chat.Turn{Prompt: "more"}); err != nil {
		t.Fatal(err)
	}
	if live() != 1 {
		t.Fatal("send did not reopen the chat")
	}
}

// Every row a phone reads names its model through the catalog, since the
// cache stores only the id.
func TestRowsCarryTheModelLabel(t *testing.T) {
	label := func(kind agent.Kind, model string) string { return string(kind) + " names " + model }
	svc, _, _ := newService(t, agent.KindClaude, func(d *chat.Deps) { d.ModelLabel = label })
	if err := svc.Cache.Sync(session.Meta{
		Agent: agent.KindClaude, ID: "sess-1", Title: "demo", Cwd: "/tmp/project",
		UpdatedAt: time.UnixMilli(2000), SizeBytes: 2, Model: "claude-opus-5-5",
	}, nil); err != nil {
		t.Fatal(err)
	}
	const want = "claude names claude-opus-5-5"

	info, err := svc.Info("claude:sess-1")
	if err != nil || info.ModelLabel != want {
		t.Fatalf("info label = %q, %v; want %q", info.ModelLabel, err, want)
	}
	page, err := svc.List(t.Context(), store.ChatQuery{}, false)
	if err != nil || len(page.Chats) != 1 || page.Chats[0].ModelLabel != want {
		t.Fatalf("list = %+v, %v; want one row labelled %q", page.Chats, err, want)
	}
}

// A new title goes to the provider's files and shows in the row at once,
// with its spacing collapsed to one line.
func TestUpdateTitleWritesTheTranscriptAndTheRow(t *testing.T) {
	svc, _, files := newService(t, agent.KindClaude)
	title := "  Fix the\n login bug "
	c, err := svc.Update("claude:sess-1", chat.Update{Title: &title})
	if err != nil {
		t.Fatal(err)
	}
	if c.Title != "Fix the login bug" {
		t.Errorf("title = %q", c.Title)
	}
	if len(files.renamed) != 1 || files.renamed[0] != "sess-1: Fix the login bug" {
		t.Errorf("transcript renames = %v", files.renamed)
	}
}

func TestUpdateRefusesAnEmptyTitleAndAnUnknownChat(t *testing.T) {
	svc, _, files := newService(t, agent.KindClaude)
	empty, title := "   ", "Title"
	if _, err := svc.Update("claude:sess-1", chat.Update{Title: &empty}); errkind.Of(err) != errkind.Invalid {
		t.Errorf("empty title: %v, want invalid", err)
	}
	if _, err := svc.Update("claude:nope", chat.Update{Title: &title}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown chat: %v, want not found", err)
	}
	if len(files.renamed) != 0 {
		t.Errorf("transcript renames = %v", files.renamed)
	}
}

func TestDeleteRemovesTranscriptThenRowAndAnnounces(t *testing.T) {
	svc, _, files := newService(t, agent.KindClaude)
	var removed []store.ChatID
	svc.Cache.Notify(func(c store.Change) { removed = append(removed, c.Removed...) })

	if err := svc.Delete("claude:sess-1"); err != nil {
		t.Fatal(err)
	}
	if len(files.deleted) != 1 || files.deleted[0] != "sess-1" {
		t.Fatalf("transcript deletions = %v", files.deleted)
	}
	if _, err := svc.Cache.Info("claude:sess-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("row after delete: %v", err)
	}
	if len(removed) != 1 || removed[0] != "claude:sess-1" {
		t.Fatalf("announced = %v", removed)
	}
	if err := svc.Delete("claude:sess-1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("second delete = %v, want not found", err)
	}
}

func TestDeleteRefusesAFreshlyActiveChat(t *testing.T) {
	svc, _, files := newService(t, agent.KindClaude)
	if err := svc.Cache.SetStatus("claude:sess-1", "/tmp/project", agent.ChatWorking, time.Now(), store.TurnObservation{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete("claude:sess-1"); !(errkind.Of(err) == errkind.Invalid) {
		t.Fatalf("delete of a working chat = %v, want invalid", err)
	}
	if len(files.deleted) != 0 {
		t.Fatal("transcript removed despite the refusal")
	}
}

// A status nothing has renewed is a turn that ended without saying so.
func TestDeleteIgnoresAStaleActiveStatus(t *testing.T) {
	svc, _, files := newService(t, agent.KindCodex)
	if err := svc.Cache.SetStatus("codex:sess-1", "/tmp/project", agent.ChatWorking, time.Now().Add(-10*time.Minute), store.TurnObservation{}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete("codex:sess-1"); err != nil {
		t.Fatalf("delete of a stale working chat = %v", err)
	}
	if len(files.deleted) != 1 {
		t.Fatalf("transcript deletions = %v", files.deleted)
	}
}

// The host's own turn is running however old the row's status is.
func TestDeleteRefusesAChatThisHostIsRunning(t *testing.T) {
	svc, rec, files := newService(t, agent.KindCodex)
	rec.live = []agent.TurnStatus{
		{TurnID: "done", ChatID: "codex:sess-1", State: agent.StateDone},
		{TurnID: "live", ChatID: "codex:sess-1", State: agent.StateRunning},
	}
	if err := svc.Delete("codex:sess-1"); !(errkind.Of(err) == errkind.Invalid) {
		t.Fatalf("delete during a running turn = %v, want invalid", err)
	}
	if len(files.deleted) != 0 {
		t.Fatal("transcript removed despite the refusal")
	}
}

func TestSubagentReadsTheChatsSession(t *testing.T) {
	svc, _, files := newService(t, agent.KindClaude)
	sub, err := svc.Subagent("claude:sess-1", "toolu_task")
	if err != nil {
		t.Fatal(err)
	}
	if files.sessionID != "sess-1" || sub.Kind != "Explore" || len(sub.Events) != 1 {
		t.Fatalf("read %q: %+v", files.sessionID, sub)
	}
	if _, err := svc.Subagent("claude:sess-1", "toolu_x"); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("unknown call = %v, want not found", err)
	}
	for _, id := range []store.ChatID{"claude:", "nocolon"} {
		if _, err := svc.Subagent(id, "toolu_task"); !(errkind.Of(err) == errkind.Invalid) {
			t.Fatalf("%q = %v, want invalid", id, err)
		}
	}
	if _, err := svc.Subagent("claude:sess-1", " "); !(errkind.Of(err) == errkind.Invalid) {
		t.Fatalf("no call id = %v, want invalid", err)
	}
}

// A new chat's title rides with its first turn on one line; one past the row's
// limit is refused before anything is queued.
func TestStartCarriesTheTitle(t *testing.T) {
	h := testhost.New(t)
	d := h.ChatDeps
	rec := &recorder{}
	d.Sender, d.Transcripts = rec, &transcripts{}
	svc, err := chat.New(d)
	if err != nil {
		t.Fatal(err)
	}
	turn := chat.Turn{Prompt: "can you fix the build please"}
	if _, err := svc.Start(chat.Start{Path: h.Root, Agent: string(agent.KindClaude), Title: " Fix the\nBuild "}, turn); err != nil {
		t.Fatal(err)
	}
	if rec.last.Title != "Fix the Build" {
		t.Fatalf("queued title %q, want it on one line", rec.last.Title)
	}
	_, err = svc.Start(chat.Start{Path: h.Root, Agent: string(agent.KindClaude), Title: strings.Repeat("a", 121)}, turn)
	if !errors.Is(err, errkind.ErrInvalid) || rec.n != 1 {
		t.Fatalf("over-long title: err %v after %d sends, want ErrInvalid and nothing queued", err, rec.n)
	}
}
