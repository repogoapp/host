package chat_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/chat"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/session"
	"github.com/repogoapp/host/internal/store"
	"github.com/repogoapp/host/internal/testhost"
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

func (r *recorder) StopChat(string) error { return agent.ErrNotFound }

func (r *recorder) Wait(context.Context, string) (agent.TurnStatus, string, error) {
	return agent.TurnStatus{}, "", agent.ErrNotFound
}

// transcripts is a Transcripts that remembers deletions and renames.
type transcripts struct {
	deleted []string
	renamed []string
}

func (f *transcripts) Delete(sessionID string) error {
	f.deleted = append(f.deleted, sessionID)
	return nil
}

func (f *transcripts) Rename(sessionID, title string) error {
	f.renamed = append(f.renamed, sessionID+": "+title)
	return nil
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

// A subagent is read from the cache, beside its chat: a call's only agent, or
// one of a workflow's by key.
func TestSubagentReadsTheCache(t *testing.T) {
	svc, _, _ := newService(t, agent.KindClaude)
	parent := session.Meta{Agent: agent.KindClaude, ID: "sess-1"}
	children := []session.Child{
		{Key: "a9", CallID: "toolu_task", Kind: "Explore"},
		{Key: "w1", CallID: "toolu_wf", RunID: "wf_1", Label: "map:math", Phase: "Map", Order: 0, Done: true},
		{Key: "w2", CallID: "toolu_wf", RunID: "wf_1", Label: "build", Phase: "Build", Order: 1},
	}
	events := map[string][]agent.Event{
		"a9": {{Kind: agent.EventText, Text: "It is in claude.go."}},
		"w2": {{Kind: agent.EventText, Text: "Working on it."}},
	}
	if _, err := svc.Cache.SyncSubagents(parent, children, events); err != nil {
		t.Fatal(err)
	}

	sub, rows, err := svc.Subagent("claude:sess-1", "toolu_task", "")
	if err != nil {
		t.Fatal(err)
	}
	if sub.Kind != "Explore" || len(rows) != 1 || rows[0].Text != "It is in claude.go." {
		t.Fatalf("subagent = %+v, %+v", sub, rows)
	}
	if _, rows, err := svc.Subagent("claude:sess-1", "toolu_wf", "w2"); err != nil || len(rows) != 1 || rows[0].Text != "Working on it." {
		t.Fatalf("workflow agent w2 = %+v, %v", rows, err)
	}
	agents, err := svc.Subagents("claude:sess-1", "toolu_wf")
	if err != nil || len(agents) != 2 || agents[0].Key != "w1" || !agents[0].Done || agents[1].Phase != "Build" {
		t.Fatalf("workflow agents = %+v, %v", agents, err)
	}

	if _, _, err := svc.Subagent("claude:sess-1", "toolu_x", ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown call = %v, want not found", err)
	}
	for _, id := range []store.ChatID{"claude:", "nocolon"} {
		if _, _, err := svc.Subagent(id, "toolu_task", ""); !(errkind.Of(err) == errkind.Invalid) {
			t.Fatalf("%q = %v, want invalid", id, err)
		}
	}
	if _, _, err := svc.Subagent("claude:sess-1", " ", ""); !(errkind.Of(err) == errkind.Invalid) {
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

// Stopping a chat the host no longer runs settles whatever the cache shows
// working, a dead host turn or a terminal's: the chat must never be left
// unstoppable. A chat with nothing active still reports ErrNotFound.
func TestStopSettlesAChatNothingHereRuns(t *testing.T) {
	svc, _, _ := newService(t, agent.KindClaude)
	for _, key := range []string{store.ManagerTurnPrefix + "dead", "hook-prompt:p1"} {
		started := time.Now()
		if err := svc.Cache.SetStatus("claude:sess-1", "/tmp/project", agent.ChatWorking, started,
			store.TurnObservation{Key: key, StartedAt: &started}); err != nil {
			t.Fatal(err)
		}
		if err := svc.Stop("claude:sess-1"); err != nil {
			t.Fatalf("Stop of %s = %v, want the turn settled", key, err)
		}
		if info, _ := svc.Cache.Info("claude:sess-1"); info.Status != agent.ChatInterrupted {
			t.Errorf("%s: status = %s, want interrupted", key, info.Status)
		}
	}
	if err := svc.Stop("claude:sess-1"); !errors.Is(err, agent.ErrNotFound) {
		t.Fatalf("Stop of a settled chat = %v, want ErrNotFound", err)
	}
}

// A chat an agent starts runs under the calling chat, with no broader
// permission or mode than the agent's own: unset ones take the caller's.
func TestAnAgentsStartIsAChildCappedAtItsCaller(t *testing.T) {
	h := testhost.New(t)
	d := h.ChatDeps
	rec := &recorder{}
	d.Sender, d.Transcripts = rec, &transcripts{}
	svc, err := chat.New(d)
	if err != nil {
		t.Fatal(err)
	}
	from := &chat.Caller{ChatID: "claude:parent", Config: agent.TurnConfig{PermissionMode: agent.PermissionAutoAcceptEdits, Mode: "plan"}}
	where := chat.Start{Path: h.Root, Agent: string(agent.KindCodex), Parent: "claude:parent"}

	if _, err := svc.Start(where, chat.Turn{Prompt: "review this", From: from}); err != nil {
		t.Fatal(err)
	}
	if rec.last.Parent != "claude:parent" || rec.last.Config.PermissionMode != agent.PermissionAutoAcceptEdits || rec.last.Config.Mode != "plan" {
		t.Fatalf("queued parent %q, permission %q, mode %q; want the caller's", rec.last.Parent, rec.last.Config.PermissionMode, rec.last.Config.Mode)
	}

	for _, cfg := range []agent.TurnConfig{
		{PermissionMode: agent.PermissionFullAccess},
		{Mode: "agent"},
		{PermissionMode: "anything"},
	} {
		_, err := svc.Start(where, chat.Turn{Prompt: "review this", Config: cfg, From: from})
		if !errors.Is(err, errkind.ErrDenied) {
			t.Fatalf("start with %+v: err %v, want ErrDenied", cfg, err)
		}
	}
	if rec.n != 1 {
		t.Fatalf("%d turns queued, want only the first", rec.n)
	}
}

// A child stays one whoever sends to it, so its tools stay limited.
func TestSendToAChildCarriesItsParent(t *testing.T) {
	svc, rec, _ := newService(t, agent.KindClaude)
	if err := svc.Cache.SetChatParent("claude:sess-1", "codex:parent"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Send("claude:sess-1", chat.Turn{Prompt: "and the tests?"}); err != nil {
		t.Fatal(err)
	}
	if rec.last.Parent != "codex:parent" {
		t.Fatalf("queued parent %q, want codex:parent", rec.last.Parent)
	}
}
