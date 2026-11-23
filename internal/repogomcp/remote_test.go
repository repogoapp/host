package repogomcp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/rpc"
)

var studioChat = RemoteAgent{HostID: "H-MACBOOK", ChatID: "claude:a1", PermissionMode: "default"}

func TestRemoteCallRunsAHostToolAsTheCallingChat(t *testing.T) {
	calls := map[string]rpc.Caller{}
	s := hostToolsServer(t, hostRouter(t, calls))
	out, err := s.RemoteCall(context.Background(), studioChat, "chats.pin", json.RawMessage(`{"chat_id": "codex:b2", "pinned": true}`))
	if err != nil {
		t.Fatal(err)
	}
	var got pinParams
	if json.Unmarshal(out, &got) != nil || got.ChatID != "codex:b2" || !got.Pinned {
		t.Fatalf("result %s, want the pin echoed", out)
	}
	c := calls["chats.pin"]
	if c.Agent == nil || c.Agent.ChatID != "claude:a1" || c.Agent.PermissionMode != "default" || c.Scope != rpc.ScopeLocal || c.Device != "" {
		t.Fatalf("caller %+v, want the remote chat as a local agent with no device", c)
	}
}

func TestRemoteCallReachesOnlyTools(t *testing.T) {
	calls := map[string]rpc.Caller{}
	s := hostToolsServer(t, hostRouter(t, calls))
	for _, method := range []string{"fs.delete", "actions.run", "nope.nothing"} {
		if _, err := s.RemoteCall(context.Background(), studioChat, method, nil); !errors.Is(err, errkind.ErrInvalid) {
			t.Errorf("%s: err %v, want invalid", method, err)
		}
	}
	if _, ran := calls["fs.delete"]; ran {
		t.Fatal("fs.delete ran for another host's agent")
	}
}

func TestRemoteCallCantStartOrMessageChatsYet(t *testing.T) {
	calls := map[string]rpc.Caller{}
	s := hostToolsServer(t, hostRouter(t, calls))
	if _, err := s.RemoteCall(context.Background(), studioChat, "chats.send", json.RawMessage(`{"chat_id": "codex:b2"}`)); !errors.Is(err, errkind.ErrUnavailable) {
		t.Fatalf("chats.send: err %v, want unavailable", err)
	}
	if _, ran := calls["chats.send"]; ran {
		t.Fatal("chats.send ran for another host's agent")
	}
}

func TestRemoteCallNeedsTheCallingChat(t *testing.T) {
	s := hostToolsServer(t, hostRouter(t, map[string]rpc.Caller{}))
	if _, err := s.RemoteCall(context.Background(), RemoteAgent{HostID: "H"}, "chats.pin", nil); !errors.Is(err, errkind.ErrInvalid) {
		t.Fatalf("err %v, want invalid without a chat_id", err)
	}
}
