package chats_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/rpc/chats"
	"github.com/repogo/host/internal/session"
	"github.com/repogo/host/internal/testhost"
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
		HostID string `json:"host_id"`
		Chats  []struct {
			HostID string `json:"host_id"`
		} `json:"chats"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Chats) != 1 {
		t.Fatalf("got %d chats, want 1", len(got.Chats))
	}
	if got.HostID != host || got.Chats[0].HostID != host {
		t.Errorf("host id = %q / row %q, want %s on both", got.HostID, got.Chats[0].HostID, host)
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
