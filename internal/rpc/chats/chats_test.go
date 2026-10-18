package chats_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/chat"
	"github.com/repogoapp/host/internal/rpc"
	"github.com/repogoapp/host/internal/rpc/chats"
	"github.com/repogoapp/host/internal/session"
	"github.com/repogoapp/host/internal/testhost"
)

// Family-level tests: the testhost's services behind the chats family, with
// one chat cached. Chat behaviour itself is tested in internal/chat.
func newRouter(t *testing.T) (*rpc.Router, string) {
	t.Helper()
	th := testhost.New(t)
	if err := th.Config.Store.Sync(session.Meta{
		Agent: agent.KindClaude, ID: "sess-1", Title: "demo", Cwd: "/tmp/project",
		UpdatedAt: time.UnixMilli(1000), SizeBytes: 1,
	}, []agent.Event{{Kind: agent.EventText, Text: "hi"}}); err != nil {
		t.Fatal(err)
	}
	r := rpc.New(slog.New(slog.DiscardHandler))
	chats.Register(r, chats.Deps{Chats: th.Config.Chats, Live: th.Config.Live})
	return r, string(th.Devices.Identity().ID)
}

func call(t *testing.T, r *rpc.Router, method string, params any) (json.RawMessage, error) {
	t.Helper()
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	return r.Call(context.Background(), rpc.Caller{Device: "caller", Scope: rpc.ScopeRemote}, method, b)
}

// A client mistake is invalid params and a chat the cache lacks is not found,
// never an internal error: it is routine on a cache we are free to delete.
func TestSendErrorsReachTheWireByKind(t *testing.T) {
	r, _ := newRouter(t)
	if _, err := call(t, r, "chats.send", map[string]any{"chat_id": "claude:sess-1", "turn": map[string]any{"prompt": "   "}}); !errors.Is(err, rpc.ErrInvalidParams) {
		t.Errorf("empty prompt: %v, want invalid params", err)
	}
	if _, err := call(t, r, "chats.send", map[string]any{"chat_id": "claude:nope", "turn": map[string]any{"prompt": "hello"}}); !errors.Is(err, rpc.ErrNotFound) {
		t.Errorf("unknown chat: %v, want not found", err)
	}
}

// The host stamps which machine a chat lives on. Without it a client with two
// hosts paired cannot tell two chats at the same path apart.
func TestListStampsTheHostOnEveryRow(t *testing.T) {
	r, host := newRouter(t)
	out, err := call(t, r, "chats.list", map[string]any{"limit": 10, "search": "  demo "})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var got struct {
		Chats []struct {
			HostID string `json:"host_id"`
		} `json:"chats"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Chats) != 1 {
		t.Fatalf("got %d chats, want 1", len(got.Chats))
	}
	if got.Chats[0].HostID != host {
		t.Errorf("row host id = %q, want %s", got.Chats[0].HostID, host)
	}
}

func TestResolveAnswersTheResolvedRow(t *testing.T) {
	r, _ := newRouter(t)
	raw, err := call(t, r, "chats.resolve", map[string]any{"chat_id": "claude:sess-1", "resolved": true})
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Chat struct {
			ResolvedAt *int64 `json:"resolved_at"`
		} `json:"chat"`
	}
	if json.Unmarshal(raw, &res) != nil || res.Chat.ResolvedAt == nil {
		t.Fatalf("resolve reply = %s", raw)
	}
}

// A pin comes back on the row, so every device's list moves it at once.
func TestPinAnswersThePinnedRow(t *testing.T) {
	r, _ := newRouter(t)
	raw, err := call(t, r, "chats.pin", map[string]any{"chat_id": "claude:sess-1", "pinned": true})
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Chat struct {
			PinnedAt *int64 `json:"pinned_at"`
		} `json:"chat"`
	}
	if json.Unmarshal(raw, &res) != nil || res.Chat.PinnedAt == nil {
		t.Fatalf("pin reply = %s", raw)
	}
}

// A title with nothing in it is the client's mistake.
func TestUpdateRefusesAnEmptyTitle(t *testing.T) {
	r, _ := newRouter(t)
	if _, err := call(t, r, "chats.update", map[string]any{"chat_id": "claude:sess-1", "title": "  "}); !errors.Is(err, rpc.ErrInvalidParams) {
		t.Errorf("empty title: %v, want invalid params", err)
	}
}

// An update that sets nothing changes nothing and answers the row.
func TestUpdateWithNoFieldsAnswersTheRow(t *testing.T) {
	r, _ := newRouter(t)
	raw, err := call(t, r, "chats.update", map[string]any{"chat_id": "claude:sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	var res struct {
		Chat struct {
			Title string `json:"title"`
		} `json:"chat"`
	}
	if json.Unmarshal(raw, &res) != nil || res.Chat.Title != "demo" {
		t.Fatalf("update reply = %s", raw)
	}
}

// sender records the turns it is handed and answers waits with wait.
type sender struct {
	last agent.TurnRequest
	wait agent.TurnStatus
}

func (s *sender) Send(req agent.TurnRequest) (agent.TurnStatus, error) {
	s.last = req
	return agent.TurnStatus{TurnID: "turn-1", State: agent.StateRunning}, nil
}

func (s *sender) Steer(req agent.TurnRequest) (agent.TurnStatus, bool, error) {
	st, err := s.Send(req)
	return st, false, err
}

func (*sender) StopChat(string) error    { return agent.ErrNotFound }
func (*sender) List() []agent.TurnStatus { return nil }
func (s *sender) Wait(context.Context, string) (agent.TurnStatus, string, error) {
	return s.wait, "Looks good.", nil
}

func recordingRouter(t *testing.T) (*rpc.Router, *sender, string) {
	t.Helper()
	th := testhost.New(t)
	d := th.ChatDeps
	rec := &sender{}
	d.Sender = rec
	svc, err := chat.New(d)
	if err != nil {
		t.Fatal(err)
	}
	r := rpc.New(slog.New(slog.DiscardHandler))
	chats.Register(r, chats.Deps{Chats: svc, Live: th.Config.Live})
	return r, rec, th.Root
}

// An agent's start runs under its chat unless it asks for a chat of the
// user's own; a device's never has a parent, whatever it sends.
func TestStartFromAnAgentMakesAChild(t *testing.T) {
	r, rec, root := recordingRouter(t)
	agentCaller := rpc.Caller{Device: "phone", Scope: rpc.ScopeLocal, Agent: &rpc.AgentCaller{ChatID: "claude:parent"}}
	start := func(c rpc.Caller, child any) string {
		t.Helper()
		params := map[string]any{"cwd": root, "agent": "codex", "turn": map[string]any{"prompt": "review this"}}
		if child != nil {
			params["child"] = child
		}
		b, _ := json.Marshal(params)
		if _, err := r.Call(context.Background(), c, "chats.start", b); err != nil {
			t.Fatal(err)
		}
		return rec.last.Parent
	}
	if got := start(agentCaller, nil); got != "claude:parent" {
		t.Errorf("an agent's start has parent %q, want claude:parent", got)
	}
	if got := start(agentCaller, false); got != "" {
		t.Errorf("child false has parent %q, want none", got)
	}
	if got := start(rpc.Caller{Device: "phone", Scope: rpc.ScopeRemote}, true); got != "" {
		t.Errorf("a device's start has parent %q, want none", got)
	}
}

// chats.wait names the chat only once the agent has, and carries its answer.
func TestWaitNamesTheChatOnceItHasAnID(t *testing.T) {
	r, rec, _ := recordingRouter(t)
	rec.wait = agent.TurnStatus{TurnID: "turn-1", ChatID: "6f1c2a", State: agent.StateRunning}
	out, err := call(t, r, "chats.wait", map[string]any{"turn_id": "turn-1"})
	if err != nil {
		t.Fatal(err)
	}
	var got chats.WaitResult
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.ChatID != "" || got.Reply != "Looks good." {
		t.Fatalf("before the id: %+v", got)
	}
	rec.wait = agent.TurnStatus{TurnID: "turn-1", ChatID: "codex:abc", State: agent.StateDone}
	out, err = call(t, r, "chats.wait", map[string]any{"turn_id": "turn-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.ChatID != "codex:abc" || got.State != agent.StateDone {
		t.Fatalf("after the id: %+v", got)
	}
}
