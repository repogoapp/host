package repogomcp

import (
	"context"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/rpc"
)

type pinParams struct {
	ChatID string `json:"chat_id"`
	Pinned bool   `json:"pinned"`
}

// hostRouter serves chats.pin and chats.send as the chats family does, and
// fs.delete, which is not a Tool; calls records each method's caller.
func hostRouter(t *testing.T, calls map[string]rpc.Caller) *rpc.Router {
	t.Helper()
	r := rpc.New(nil)
	rpc.Add(r, "chats.pin", func(_ context.Context, c rpc.Caller, p pinParams) (pinParams, error) {
		calls["chats.pin"] = c
		return p, nil
	}, rpc.Tool("Pin a chat."), rpc.Write)
	rpc.Add(r, "chats.send", func(_ context.Context, c rpc.Caller, p pinParams) (rpc.Ack, error) {
		calls["chats.send"] = c
		return rpc.OK, nil
	}, rpc.Tool("Send a message."), rpc.Write)
	rpc.Add(r, "fs.delete", func(_ context.Context, c rpc.Caller, _ rpc.None) (rpc.Ack, error) {
		calls["fs.delete"] = c
		return rpc.OK, nil
	})
	return r
}

func hostToolsServer(t *testing.T, router *rpc.Router) *Server {
	t.Helper()
	return New(Config{
		URL: func() string { return "http://127.0.0.1:1/mcp/repogo" }, On: func(string) bool { return true },
		Browser: newFakePhone(true, nil).browser, Plugins: pluginsService(t, ""), MCP: mcpServiceFunc(t),
		Router: func() *rpc.Router { return router },
	})
}

// Every method in the embedded file is listed, as chats_<method>, beside browser.
func TestHostToolsAreListed(t *testing.T) {
	s := hostToolsServer(t, hostRouter(t, map[string]rpc.Caller{}))
	names := toolNames(t, s, attachIn(t, s, t.TempDir()))
	for _, want := range []string{"browser", "chats_list", "chats_update", "chats_send"} {
		if !strings.Contains(","+names+",", ","+want+",") {
			t.Errorf("%s missing from %s", want, names)
		}
	}
	if strings.Contains(names, "chats_delete") {
		t.Errorf("chats.delete is not a Tool: %s", names)
	}
}

// A host tool runs its method for the phone that sent the turn, and answers
// with the method's own result.
func TestHostToolCallsTheMethod(t *testing.T) {
	calls := map[string]rpc.Caller{}
	s := hostToolsServer(t, hostRouter(t, calls))
	got := callTool(t, s, attachIn(t, s, t.TempDir()), "chats_pin", map[string]any{"chat_id": "codex:other", "pinned": true})
	if got.IsError || len(got.Content) != 1 || !strings.Contains(got.Content[0].Text, `"pinned": true`) {
		t.Fatalf("result %+v", got)
	}
	if c := calls["chats.pin"]; c.Device != device.ID("phone") {
		t.Fatalf("caller %+v", c)
	}
}

// The embedded file is the allowlist: a method on the router that is not a
// Tool can't be reached by its tool name.
func TestHostToolsReachOnlyTools(t *testing.T) {
	calls := map[string]rpc.Caller{}
	s := hostToolsServer(t, hostRouter(t, calls))
	got := callTool(t, s, attachIn(t, s, t.TempDir()), "fs_delete", map[string]any{})
	if !got.IsError {
		t.Fatalf("fs_delete answered %+v", got)
	}
	if _, ran := calls["fs.delete"]; ran {
		t.Fatal("fs.delete ran")
	}
}

// A turn can't message its own chat, and the method never runs; another chat is fine.
func TestHostToolsRefuseTheCallingChat(t *testing.T) {
	calls := map[string]rpc.Caller{}
	s := hostToolsServer(t, hostRouter(t, calls))
	token := attachIn(t, s, t.TempDir())
	got := callTool(t, s, token, "chats_send", map[string]any{"chat_id": "claude:s1"})
	if !got.IsError || !strings.Contains(got.Content[0].Text, "this chat") {
		t.Fatalf("own chat answered %+v", got)
	}
	if _, ran := calls["chats.send"]; ran {
		t.Fatal("chats.send ran for its own chat")
	}
	if got := callTool(t, s, token, "chats_send", map[string]any{"chat_id": "claude:s2"}); got.IsError {
		t.Fatalf("another chat refused: %+v", got)
	}
}

// A host without the router (still assembling) offers none of them.
func TestHostToolsNeedTheRouter(t *testing.T) {
	s := hostToolsServer(t, nil)
	if names := toolNames(t, s, attachIn(t, s, t.TempDir())); strings.Contains(names, "chats_") {
		t.Fatalf("tools %s", names)
	}
}

// A host tool's method hears which chat called and what its turn runs with,
// the limits on what it may start or send.
func TestHostToolsNameTheCallingChat(t *testing.T) {
	calls := map[string]rpc.Caller{}
	s := hostToolsServer(t, hostRouter(t, calls))
	turn := agent.RunningTurn{Ctx: t.Context(), TurnID: "t1", ChatID: "claude:s1", Cwd: t.TempDir(), Device: "phone",
		Config: agent.TurnConfig{PermissionMode: agent.PermissionAutoAcceptEdits, Mode: "plan"}}
	token, release := bearer(t, s, &fakeSession{turn: turn})
	defer release()
	if got := callTool(t, s, token, "chats_pin", map[string]any{"chat_id": "codex:other", "pinned": true}); got.IsError {
		t.Fatalf("result %+v", got)
	}
	want := rpc.AgentCaller{ChatID: "claude:s1", PermissionMode: "auto-accept-edits", Mode: "plan"}
	if c := calls["chats.pin"]; c.Agent == nil || *c.Agent != want {
		t.Fatalf("caller's agent %+v, want %+v", c.Agent, want)
	}
}

// A chat another chat's agent started can't start or message chats: neither
// tool is listed, and a call by name is refused before the method runs. It
// keeps the rest.
func TestAChildCantStartOrMessageChats(t *testing.T) {
	calls := map[string]rpc.Caller{}
	s := hostToolsServer(t, hostRouter(t, calls))
	cwd := t.TempDir()
	turn := agent.RunningTurn{Ctx: t.Context(), TurnID: "t1", ChatID: "codex:child", Cwd: cwd, Device: "phone", Parent: "claude:parent"}
	entry, release := s.Attach(&fakeSession{turn: turn}, agent.TurnRequest{Cwd: cwd, Parent: "claude:parent"})
	defer release()
	token := strings.TrimPrefix(entry.Headers[0].Value, "Bearer ")

	names := "," + toolNames(t, s, token) + ","
	if strings.Contains(names, ",chats_start,") || strings.Contains(names, ",chats_send,") || !strings.Contains(names, ",chats_list,") {
		t.Fatalf("a child's tools: %s", names)
	}
	for _, name := range []string{"chats_start", "chats_send"} {
		got := callTool(t, s, token, name, map[string]any{"chat_id": "claude:other"})
		if !got.IsError || !strings.Contains(got.Content[0].Text, "another chat started this one") {
			t.Fatalf("%s from a child answered %+v", name, got)
		}
	}
	if _, ran := calls["chats.send"]; ran {
		t.Fatal("chats.send ran for a child")
	}
	if got := callTool(t, s, token, "chats_pin", map[string]any{"chat_id": "claude:other", "pinned": true}); got.IsError {
		t.Fatalf("a child's other tools: %+v", got)
	}
}
