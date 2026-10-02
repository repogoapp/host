package repogomcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/mcp"
)

// fakeSession is an agent process on a turn from phone, or between turns.
type fakeSession struct {
	turn agent.RunningTurn
	idle bool
}

func (f *fakeSession) Running() (agent.RunningTurn, bool) { return f.turn, !f.idle }

// fakePhone answers every request it is sent with answer, and records them.
type fakePhone struct {
	mu        sync.Mutex
	browser   *Browser
	connected bool
	sent      []Request
	alerted   []Request
	answer    func(Request) Result
}

func newFakePhone(connected bool, answer func(Request) Result) *fakePhone {
	p := &fakePhone{connected: connected, answer: answer}
	p.browser = NewBrowser(BrowserConfig{
		Phone: func(d device.ID) bool { return d == "phone" || d == "other" },
		Send: func(d device.ID, req Request) bool {
			p.mu.Lock()
			defer p.mu.Unlock()
			if !p.connected {
				return false
			}
			p.sent = append(p.sent, req)
			if p.answer != nil {
				go func() { _ = p.browser.Respond(d, req.RequestID, p.answer(req)) }()
			}
			return true
		},
		Alert: func(_ device.ID, req Request) {
			p.mu.Lock()
			defer p.mu.Unlock()
			p.alerted = append(p.alerted, req)
		},
	})
	return p
}

func turnFrom(d device.ID) agent.RunningTurn {
	return agent.RunningTurn{Ctx: context.Background(), TurnID: "t1", ChatID: "claude:s1", Cwd: "/work/app", Device: d}
}

func newServer(t *testing.T, phone *fakePhone) *Server {
	t.Helper()
	return New(Config{
		URL: func() string { return "http://127.0.0.1:1/mcp/repogo" }, On: func(string) bool { return true },
		Browser: phone.browser,
	})
}

func post(t *testing.T, h http.Handler, bearer, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, Path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func bearer(t *testing.T, s *Server, session agent.ToolSession) (string, func()) {
	t.Helper()
	entry, release := s.Attach(session)
	if entry.Name != agent.ToolsServer || entry.Type != "http" || len(entry.Headers) != 1 {
		t.Fatalf("entry %+v", entry)
	}
	return strings.TrimPrefix(entry.Headers[0].Value, "Bearer "), release
}

// The switch, the session entry, and the name the tools are called by are
// one name; a connected server cannot take it.
func TestTheServerNameIsTheBuiltinID(t *testing.T) {
	if agent.ToolsServer != mcp.BuiltinID {
		t.Fatalf("agent calls it %q, the MCP list %q", agent.ToolsServer, mcp.BuiltinID)
	}
}

// Only a session's own bearer gets in, and not after the session closes.
func TestBearerNamesALiveSession(t *testing.T) {
	s := newServer(t, newFakePhone(true, nil))
	token, release := bearer(t, s, &fakeSession{turn: turnFrom("phone")})
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`

	if w := post(t, s, "", init); w.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d", w.Code)
	}
	if w := post(t, s, "nope", init); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer: %d", w.Code)
	}
	w := post(t, s, token, init)
	var got struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("initialize: %d %s", w.Code, w.Body)
	}
	if got.Result.ProtocolVersion != "2025-03-26" || got.Result.ServerInfo.Name != "repogo" {
		t.Fatalf("initialize: %+v", got.Result)
	}
	if w := post(t, s, token, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); w.Code != http.StatusAccepted {
		t.Fatalf("notification: %d", w.Code)
	}
	get := httptest.NewRequest(http.MethodGet, Path, nil)
	get.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, get)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", rec.Code)
	}

	release()
	if w := post(t, s, token, init); w.Code != http.StatusUnauthorized {
		t.Fatalf("after release: %d", w.Code)
	}
}

func TestToolsListNamesBrowserAndBuilds(t *testing.T) {
	s := newServer(t, newFakePhone(true, nil))
	token, _ := bearer(t, s, &fakeSession{turn: turnFrom("phone")})
	w := post(t, s, token, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var got struct {
		Result struct {
			Tools []struct {
				Name        string         `json:"name"`
				InputSchema map[string]any `json:"inputSchema"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range got.Result.Tools {
		if tool.InputSchema["type"] != "object" {
			t.Errorf("%s has no object schema", tool.Name)
		}
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "browser,build,build_status" {
		t.Fatalf("tools %v", names)
	}
}

type callResult struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func callTool(t *testing.T, s *Server, token, name string, args any) callResult {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	w := post(t, s, token, string(body))
	var got struct {
		Result callResult `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("%s: %s", err, w.Body)
	}
	return got.Result
}

// A snapshot goes to the phone that sent the turn, in the turn's workspace,
// and comes back as v1's JSON plus the screenshot as an image.
func TestBrowserSnapshotRoundTrip(t *testing.T) {
	phone := newFakePhone(true, func(req Request) Result {
		return Result{
			OK: true, URL: "http://localhost:3000/", Title: "Home", Snapshot: "- button \"Go\" [ref=e1]",
			SnapshotKind: "a11y", Screenshot: &Screenshot{MimeType: "image/jpeg", Data: "AAAA"},
		}
	})
	s := newServer(t, phone)
	token, _ := bearer(t, s, &fakeSession{turn: turnFrom("phone")})

	got := callTool(t, s, token, "browser", map[string]any{"action": "snapshot", "screenshot": true})
	if got.IsError || len(got.Content) != 2 {
		t.Fatalf("result %+v", got)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got.Content[0].Text), &out); err != nil {
		t.Fatal(err)
	}
	if out["action"] != "snapshot" || out["snapshot"] != "- button \"Go\" [ref=e1]" || out["url"] != "http://localhost:3000/" {
		t.Fatalf("payload %v", out)
	}
	if got.Content[1].Type != "image" || got.Content[1].MimeType != "image/jpeg" || got.Content[1].Data != "AAAA" {
		t.Fatalf("image %+v", got.Content[1])
	}
	if len(phone.sent) != 1 {
		t.Fatalf("sent %d requests", len(phone.sent))
	}
	req := phone.sent[0]
	if req.Cwd != "/work/app" || req.TurnID != "t1" || req.Action.Action != "snapshot" ||
		req.Action.Format != "a11y" || !req.Action.IncludeScreenshot || req.Action.TimeoutMS != defaultTimeoutMS {
		t.Fatalf("request %+v", req)
	}
}

// A failed semantic action names what it found, so the agent can react.
func TestBrowserFailureDescribesTheTarget(t *testing.T) {
	phone := newFakePhone(true, func(Request) Result {
		return Result{ErrorText: "not_enabled", Target: &ElementState{Role: "button", Name: "Pay", Disabled: true}}
	})
	s := newServer(t, phone)
	token, _ := bearer(t, s, &fakeSession{turn: turnFrom("phone")})
	got := callTool(t, s, token, "browser", map[string]any{"action": "click", "locator": "e3"})
	if !got.IsError || got.Content[0].Text != `not_enabled (target: button "Pay" disabled)` {
		t.Fatalf("result %+v", got)
	}
}

// Bad calls never reach the phone; neither does a turn with no phone or a
// session between turns.
func TestBrowserRefusalsStayOnTheHost(t *testing.T) {
	phone := newFakePhone(true, func(Request) Result { return Result{OK: true} })
	s := newServer(t, phone)
	token, _ := bearer(t, s, &fakeSession{turn: turnFrom("phone")})
	for _, tc := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"action": "fly"}, "unsupported browser action: fly"},
		{map[string]any{"action": "click"}, "action 'click' needs a locator, or viewport x and y"},
		{map[string]any{"action": "evaluate"}, "script is required for action 'evaluate'"},
		{map[string]any{"action": "scroll"}, "deltaX and/or deltaY is required for action 'scroll'"},
		{map[string]any{"action": "close"}, "browserId is required for action 'close' (the id from action 'list'/'open')"},
	} {
		if got := callTool(t, s, token, "browser", tc.args); !got.IsError || got.Content[0].Text != tc.want {
			t.Errorf("%v: %+v", tc.args, got)
		}
	}

	host, _ := bearer(t, s, &fakeSession{turn: turnFrom("")})
	if got := callTool(t, s, host, "browser", map[string]any{"action": "list"}); got.Content[0].Text != errNoPhone {
		t.Errorf("a host turn: %+v", got)
	}
	idle, _ := bearer(t, s, &fakeSession{idle: true})
	if got := callTool(t, s, idle, "browser", map[string]any{"action": "list"}); !got.IsError {
		t.Errorf("between turns: %+v", got)
	}
	if len(phone.sent) != 0 {
		t.Fatalf("the phone was sent %d requests", len(phone.sent))
	}
}

// A click at a point keeps x and y even at the origin, and drops what the
// action does not read.
func TestBrowserActionKeepsOnlyWhatItUses(t *testing.T) {
	zero := 0.0
	a, problem := browserAction(browserInput{Action: "click", X: &zero, Y: &zero, Script: "x", URL: "u", IncludeConsole: true})
	if problem != "" || a.X != 0 || a.Script != "" || a.URL != "" || !a.IncludeConsole {
		t.Fatalf("%+v %q", a, problem)
	}
	a, _ = browserAction(browserInput{Action: "list", BrowserID: "b", IncludeConsole: true, Locator: "e1"})
	if a.IncludeConsole || a.Locator != "" || a.BrowserID != "b" {
		t.Fatalf("list %+v", a)
	}
}

// A phone that is not connected is alerted and can still answer when it
// comes back, through browser.pending; only that phone may answer.
func TestAnAwayPhoneIsAlertedAndAnswersFromPending(t *testing.T) {
	phone := newFakePhone(false, nil)
	done := make(chan Result, 1)
	go func() {
		r, _ := phone.browser.Do(turnFrom("phone"), Action{Action: "list", TimeoutMS: 1000})
		done <- r
	}()

	var pending []Request
	for len(pending) == 0 {
		pending = phone.browser.Pending("phone")
	}
	if len(phone.browser.Pending("other")) != 0 {
		t.Fatal("another phone sees the request")
	}
	if err := phone.browser.Respond("other", pending[0].RequestID, Result{OK: true}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another phone answered: %v", err)
	}
	if err := phone.browser.Respond("phone", pending[0].RequestID, Result{OK: true, Tabs: []Tab{{ID: "b1"}}}); err != nil {
		t.Fatal(err)
	}
	if r := <-done; !r.OK || len(r.Tabs) != 1 {
		t.Fatalf("result %+v", r)
	}
	phone.mu.Lock()
	defer phone.mu.Unlock()
	if len(phone.alerted) != 1 {
		t.Fatalf("alerted %d times", len(phone.alerted))
	}
	if len(phone.browser.Pending("phone")) != 0 {
		t.Fatal("an answered request is still pending")
	}
}

// Stopping the turn stops the wait.
func TestAStoppedTurnStopsWaiting(t *testing.T) {
	phone := newFakePhone(true, nil)
	ctx, cancel := context.WithCancel(context.Background())
	turn := turnFrom("phone")
	turn.Ctx = ctx
	done := make(chan string, 1)
	go func() {
		_, problem := phone.browser.Do(turn, Action{Action: "list", TimeoutMS: 60_000})
		done <- problem
	}()
	for len(phone.browser.Pending("phone")) == 0 {
	}
	cancel()
	if problem := <-done; problem != "RepoGo: the turn was stopped" {
		t.Fatalf("problem %q", problem)
	}
}
