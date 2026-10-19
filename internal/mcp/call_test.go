package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// toolServer is an MCP server with three tools: a read, a harmless write and a
// destructive one. sse answers in event streams; expire forgets the first session.
type toolServer struct {
	*httptest.Server
	sse          bool
	expire       atomic.Bool
	initializes  atomic.Int32
	calls        atomic.Int32
	lastArgument atomic.Value
}

func newToolServer(t *testing.T, sse bool) *toolServer {
	ts := &toolServer{sse: sse}
	ts.Server = httptest.NewServer(http.HandlerFunc(ts.serve))
	t.Cleanup(ts.Close)
	return ts
}

func (ts *toolServer) serve(w http.ResponseWriter, r *http.Request) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if msg.Method != "initialize" && r.Header.Get("Mcp-Session-Id") != fmt.Sprint("s", ts.initializes.Load()) {
		http.Error(w, "no session", http.StatusBadRequest)
		return
	}
	if msg.Method != "initialize" && ts.expire.CompareAndSwap(true, false) {
		http.Error(w, "gone", http.StatusNotFound)
		return
	}
	var result string
	switch msg.Method {
	case "initialize":
		w.Header().Set("Mcp-Session-Id", fmt.Sprint("s", ts.initializes.Add(1)))
		result = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}}}`
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return
	case "tools/list":
		result = `{"tools":[
			{"name":"list_projects","annotations":{"readOnlyHint":true}},
			{"name":"rename","annotations":{"readOnlyHint":false,"destructiveHint":false}},
			{"name":"delete_branch","title":"Delete branch"},
			{"name":"fails","annotations":{"readOnlyHint":true}},
			{"name":"prose","annotations":{"readOnlyHint":true}}]}`
	case "tools/call":
		ts.calls.Add(1)
		ts.lastArgument.Store(string(msg.Params.Arguments))
		switch msg.Params.Name {
		case "list_projects":
			result = `{"content":[{"type":"text","text":"[{\"id\":\"p1\"}]"}]}`
		case "rename":
			result = `{"content":[{"type":"text","text":"ignored"}],"structuredContent":{"ok":true}}`
		case "fails":
			result = `{"content":[{"type":"text","text":"quota exceeded"}],"isError":true}`
		default:
			result = `{"content":[{"type":"text","text":"Done."}]}`
		}
	}
	reply := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, msg.ID, strings.Join(strings.Fields(result), " "))
	if ts.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\"}\n\n")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", reply)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, reply)
}

// connected is a Service with up connected as "Neon", switched on for a project.
func connected(t *testing.T, up *toolServer) (*Service, string) {
	t.Helper()
	s := open(t)
	project := t.TempDir()
	r := record{Server: Server{ID: serverID(up.URL), Label: "Neon", URL: up.URL, Projects: []string{project}}, Auth: AuthNone}
	if _, err := s.put(r); err != nil {
		t.Fatal(err)
	}
	return s, project
}

func TestCallReadsATool(t *testing.T) {
	for _, sse := range []bool{false, true} {
		up := newToolServer(t, sse)
		s, project := connected(t, up)
		got, err := s.Call(context.Background(), project, "neon", "list_projects", nil, true, false)
		if err != nil {
			t.Fatalf("sse=%v: %v", sse, err)
		}
		if got.Error != nil || string(got.Result) != `[{"id":"p1"}]` {
			t.Fatalf("sse=%v: answer = %s %+v, want the text as JSON", sse, got.Result, got.Error)
		}
		if arg := up.lastArgument.Load(); arg != "{}" {
			t.Errorf("sse=%v: arguments = %v, want {}", sse, arg)
		}
	}
}

func TestCallShapesResults(t *testing.T) {
	s, project := connected(t, newToolServer(t, false))
	ctx := context.Background()
	if got, _ := s.Call(ctx, project, "neon", "rename", json.RawMessage(`{"name":"x"}`), false, false); string(got.Result) != `{"ok":true}` {
		t.Errorf("structured content = %s, want it over the text", got.Result)
	}
	if got, _ := s.Call(ctx, project, "neon", "prose", nil, true, false); string(got.Result) != `"Done."` {
		t.Errorf("prose = %s, want a JSON string", got.Result)
	}
	if got, _ := s.Call(ctx, project, "neon", "fails", nil, true, false); got.Error == nil || got.Error.Code != "TOOL_ERROR" || got.Error.Message != "quota exceeded" {
		t.Errorf("isError = %+v, want TOOL_ERROR with its text", got.Error)
	}
}

func TestCallGuardsWrites(t *testing.T) {
	up := newToolServer(t, false)
	s, project := connected(t, up)
	ctx := context.Background()

	if got, _ := s.Call(ctx, project, "neon", "rename", nil, true, false); got.Error == nil || got.Error.Code != "NOT_READ_ONLY" {
		t.Errorf("a read of a write tool = %+v, want NOT_READ_ONLY", got.Error)
	}
	got, err := s.Call(ctx, project, "neon", "delete_branch", nil, false, false)
	if err != nil || got.Error == nil || got.Error.Code != "CONFIRM" || got.Error.Message != "Run Delete branch on Neon?" {
		t.Fatalf("an unannotated write = %+v, %v; want CONFIRM, as MCP defaults a write to destructive", got.Error, err)
	}
	if up.calls.Load() != 0 {
		t.Fatalf("%d tools ran before the user agreed", up.calls.Load())
	}
	if got, _ := s.Call(ctx, project, "neon", "delete_branch", nil, false, true); got.Error != nil {
		t.Errorf("confirmed = %+v, want it run", got.Error)
	}
	if got, _ := s.Call(ctx, project, "neon", "rename", nil, false, false); got.Error != nil {
		t.Errorf("a non-destructive write = %+v, want it run without asking", got.Error)
	}
}

func TestCallOpensANewSessionWhenTheServerForgetsIt(t *testing.T) {
	up := newToolServer(t, false)
	s, project := connected(t, up)
	ctx := context.Background()
	if _, err := s.Call(ctx, project, "neon", "list_projects", nil, true, false); err != nil {
		t.Fatal(err)
	}
	up.expire.Store(true)
	got, err := s.Call(ctx, project, "neon", "list_projects", nil, true, false)
	if err != nil || got.Error != nil {
		t.Fatalf("after expiry: %+v, %v", got.Error, err)
	}
	if n := up.initializes.Load(); n != 2 {
		t.Fatalf("initialized %d times, want once more after the 404", n)
	}
}

func TestCallReachesOnlyServersOnForTheProject(t *testing.T) {
	up := newToolServer(t, false)
	s, _ := connected(t, up)
	ctx := context.Background()
	if _, err := s.Call(ctx, t.TempDir(), "neon", "list_projects", nil, true, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another project = %v, want not found", err)
	}
	_, project := connected(t, up)
	if _, err := s.Call(ctx, project, "neon", "missing", nil, true, false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown tool = %v, want not found", err)
	}
}

func TestKeysNameOnlyCallableServers(t *testing.T) {
	s, project := connected(t, newToolServer(t, false))
	if got := s.Keys(project); len(got) != 1 || got[0] != "neon" {
		t.Errorf("keys = %v, want [neon]", got)
	}
	if got := s.Keys(t.TempDir()); len(got) != 0 {
		t.Errorf("a project nothing is switched on for has %v", got)
	}
	locked := record{Server: Server{ID: "x", Label: "Locked", URL: "https://example.com", Projects: []string{project}}, Auth: AuthAPIKey}
	if _, err := s.put(locked); err != nil {
		t.Fatal(err)
	}
	if got := s.Keys(project); len(got) != 1 {
		t.Errorf("a server without its key is named: %v", got)
	}
}
