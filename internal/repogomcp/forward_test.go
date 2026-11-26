package repogomcp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/remotehost"
	"github.com/repogoapp/host/internal/rpc"
)

// fakeStudio is another host, "studio", reachable over a fake SSH: its status
// check answers ready, and each call is recorded and answered by answer.
type fakeStudio struct {
	mu     sync.Mutex
	calls  []map[string]json.RawMessage
	answer string
	// checks counts its status checks: each is this host dialing it.
	checks int
}

func (f *fakeStudio) service() *remotehost.Service {
	return remotehost.NewService(remotehost.Deps{
		Self:      "H-HERE",
		SSHConfig: func() []byte { return []byte("Host studio\n") },
		Changed:   func() {},
		Run: func(context.Context, string, string) ([]byte, []byte, int) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.checks++
			return []byte(`{"host_id": "H-STUDIO"}`), nil, 0
		},
		Send: func(_ context.Context, alias, _ string, stdin []byte) ([]byte, []byte, int) {
			var call map[string]json.RawMessage
			json.Unmarshal(stdin, &call)
			f.mu.Lock()
			f.calls = append(f.calls, call)
			f.mu.Unlock()
			if alias != "studio" {
				return nil, []byte("dialed " + alias), 255
			}
			return []byte(f.answer), nil, 0
		},
	})
}

// forwardingServer reaches studio, which the user switched on for agents.
func forwardingServer(t *testing.T, calls map[string]rpc.Caller, studio *fakeStudio) *Server {
	t.Helper()
	s, svc := listingServer(t, calls, studio)
	if _, err := svc.Authorize(t.Context(), "studio", true); err != nil {
		t.Fatal(err)
	}
	return s
}

// listingServer lists studio, which the user hasn't switched on.
func listingServer(t *testing.T, calls map[string]rpc.Caller, studio *fakeStudio) (*Server, *remotehost.Service) {
	t.Helper()
	router := hostRouter(t, calls)
	svc := studio.service()
	if _, err := svc.SetListing(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	return newForwarding(t, router, svc), svc
}

func newForwarding(t *testing.T, router *rpc.Router, svc *remotehost.Service) *Server {
	t.Helper()
	return New(Config{
		URL: func() string { return "http://127.0.0.1:1/mcp/repogo" }, On: func(string) bool { return true },
		Browser: newFakePhone(true, nil).browser, Plugins: pluginsService(t, ""), MCP: mcpServiceFunc(t),
		Router: func() *rpc.Router { return router },
		Hosts:  func() *remotehost.Service { return svc }, HostID: func() string { return "H-HERE" },
	})
}

func turnIn(t *testing.T) agent.RunningTurn {
	return agent.RunningTurn{Ctx: t.Context(), TurnID: "t1", ChatID: "claude:s1", Cwd: t.TempDir(), Device: "phone",
		Config: agent.TurnConfig{PermissionMode: agent.PermissionAutoAcceptEdits, Mode: "plan"}}
}

// toolSchemas is each listed tool's input schema properties, by name.
func toolSchemas(t *testing.T, s *Server, token string) map[string]map[string]any {
	t.Helper()
	w := post(t, s, token, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var got struct {
		Result struct {
			Tools []struct {
				Name        string `json:"name"`
				InputSchema struct {
					Properties map[string]any `json:"properties"`
				} `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, tool := range got.Result.Tools {
		out[tool.Name] = tool.InputSchema.Properties
	}
	return out
}

// A host tool another host may run takes host; one that runs only here, a
// built-in, or a host that reaches no others, doesn't.
func TestHostToolsTakeAHostWhereOneCanRunThem(t *testing.T) {
	s := forwardingServer(t, map[string]rpc.Caller{}, &fakeStudio{})
	token, release := bearer(t, s, &fakeSession{turn: turnIn(t)})
	defer release()
	schemas := toolSchemas(t, s, token)
	if _, ok := schemas["chats_pin"]["host"]; !ok {
		t.Error("chats_pin has no host")
	}
	for _, name := range []string{"chats_send", "chats_start", "hosts_list", "browser"} {
		if _, ok := schemas[name]["host"]; ok {
			t.Errorf("%s offers host, which can't run it", name)
		}
	}
	if _, ok := schemas["chats_pin"]["chat_id"]; !ok {
		t.Error("chats_pin lost its own params")
	}

	plain := hostToolsServer(t, hostRouter(t, map[string]rpc.Caller{}))
	if _, ok := toolSchemas(t, plain, attachIn(t, plain, t.TempDir()))["chats_pin"]["host"]; ok {
		t.Error("a host with no others offers host")
	}
}

// A call naming another host goes to it with the calling chat from the turn,
// the host argument taken out, and nothing runs here.
func TestACallNamingAnotherHostIsForwarded(t *testing.T) {
	calls := map[string]rpc.Caller{}
	studio := &fakeStudio{answer: `{"result": {"chat_id": "codex:b2", "pinned": true}}`}
	s := forwardingServer(t, calls, studio)
	token, release := bearer(t, s, &fakeSession{turn: turnIn(t)})
	defer release()

	got := callTool(t, s, token, "chats_pin", map[string]any{"host": "studio", "chat_id": "codex:b2", "pinned": true})
	if got.IsError || !strings.Contains(got.Content[0].Text, `"codex:b2"`) {
		t.Fatalf("forwarded result %+v", got)
	}
	if _, ran := calls["chats.pin"]; ran {
		t.Fatal("chats.pin ran here too")
	}
	if len(studio.calls) != 1 {
		t.Fatalf("studio got %d calls", len(studio.calls))
	}
	call := studio.calls[0]
	var method string
	var params map[string]any
	var from RemoteAgent
	json.Unmarshal(call["method"], &method)
	json.Unmarshal(call["params"], &params)
	json.Unmarshal(call["from"], &from)
	if method != "chats.pin" || params["chat_id"] != "codex:b2" || params["host"] != nil {
		t.Fatalf("sent %s %v, want chats.pin without host", method, params)
	}
	want := RemoteAgent{HostID: "H-HERE", ChatID: "claude:s1", PermissionMode: "auto-accept-edits", Mode: "plan"}
	if from != want {
		t.Fatalf("from %+v, want %+v", from, want)
	}
	if string(call["to"]) != `"H-STUDIO"` {
		t.Fatalf("to %s, want the host that answered when studio was checked", call["to"])
	}
}

// No host, or this host's own id, runs here; a tool that runs only here
// refuses another host rather than quietly running here; an unknown host is
// refused by name.
func TestOnlyAnotherKnownHostIsForwarded(t *testing.T) {
	calls := map[string]rpc.Caller{}
	studio := &fakeStudio{}
	s := forwardingServer(t, calls, studio)
	token, release := bearer(t, s, &fakeSession{turn: turnIn(t)})
	defer release()

	for _, args := range []map[string]any{{"chat_id": "codex:b2", "pinned": true}, {"host": "H-HERE", "chat_id": "codex:b2", "pinned": true}} {
		delete(calls, "chats.pin")
		if got := callTool(t, s, token, "chats_pin", args); got.IsError {
			t.Fatalf("%v: %+v", args, got)
		}
		if _, ran := calls["chats.pin"]; !ran {
			t.Fatalf("%v didn't run here", args)
		}
	}
	if got := callTool(t, s, token, "chats_send", map[string]any{"host": "studio", "chat_id": "codex:b2"}); !got.IsError || !strings.Contains(got.Content[0].Text, "only on the environment the chat is on") {
		t.Fatalf("a here-only tool with host: %+v", got)
	}
	if _, ran := calls["chats.send"]; ran {
		t.Fatal("chats.send ran here for a call naming studio")
	}
	if got := callTool(t, s, token, "chats_pin", map[string]any{"host": "nowhere", "chat_id": "x"}); !got.IsError || !strings.Contains(got.Content[0].Text, "nowhere isn't an environment that is switched on") {
		t.Fatalf("an unknown host: %+v", got)
	}
	if len(studio.calls) != 0 {
		t.Fatalf("studio got %d calls", len(studio.calls))
	}
}

// Another host's answer reaches the model as this host's would: a screenshot
// as a picture, a screen read one line per element.
func TestAForwardedAnswerShowsAsAHereAnswerWould(t *testing.T) {
	studio := &fakeStudio{answer: `{"result": {"path": "/remote/a.png", "image": {"mime_type": "image/jpeg", "data": "AAEC"},` +
		`"read": {"width": 402, "height": 874, "unit": "pt", "elements": [{"ref": "e1.1", "role": "Button", "label": "Files", "x": 1, "y": 2, "w": 3, "h": 4}]}}}`}
	s := forwardingServer(t, map[string]rpc.Caller{}, studio)
	token, release := bearer(t, s, &fakeSession{turn: turnIn(t)})
	defer release()

	got := callTool(t, s, token, "chats_pin", map[string]any{"host": "studio", "chat_id": "codex:b2", "pinned": true})
	if got.IsError || len(got.Content) != 2 || got.Content[1].Type != "image" || got.Content[1].Data != "AAEC" {
		t.Fatalf("forwarded result %+v", got)
	}
	if text := got.Content[0].Text; strings.Contains(text, "AAEC") || !strings.Contains(text, `e1.1 Button \"Files\" @1,2 3x4`) {
		t.Fatalf("text = %s", text)
	}
}

var pin = map[string]any{"host": "studio", "chat_id": "codex:b2", "pinned": true}

// Switching an environment on is the user's, in the app: a call to one that
// is off is refused without dialing it and without asking in the chat, in
// full access too, and says where they switch it on.
func TestACallToAHostThatIsOffIsRefusedWithoutAsking(t *testing.T) {
	studio := &fakeStudio{answer: `{"result": {"ok": true}}`}
	s, svc := listingServer(t, map[string]rpc.Caller{}, studio)
	turn := turnIn(t)
	turn.Config.PermissionMode = agent.PermissionFullAccess
	turn.Ask = func(context.Context, agent.Approval) (json.RawMessage, error) {
		t.Error("asked in the chat")
		return nil, context.Canceled
	}
	token, release := bearer(t, s, &fakeSession{turn: turn})
	defer release()
	got := callTool(t, s, token, "chats_pin", pin)
	if !got.IsError || !strings.Contains(got.Content[0].Text, "Other Environments") {
		t.Fatalf("%+v, want a refusal saying where the user switches it on", got)
	}
	if len(studio.calls)+studio.checks != 0 || svc.List(t.Context(), false).Hosts[0].Authorized {
		t.Fatalf("studio got %d calls and %d checks, want it never dialed and still off", len(studio.calls), studio.checks)
	}
}
