package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/claudecode"
)

func TestMain(m *testing.M) {
	if os.Getenv("REPOGO_FAKE_CLAUDE_RUNNER") == "1" {
		fakeRunner()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeRunner() {
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	session := ""
	for i, arg := range os.Args {
		if (arg == "--session-id" || arg == "--resume") && i+1 < len(os.Args) {
			session = os.Args[i+1]
		}
	}
	var pending string
	for scanner.Scan() {
		var frame struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			UUID      string `json:"uuid"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
			Message claudecode.Assistant `json:"message"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			os.Exit(12)
		}
		switch frame.Type {
		case "control_request":
			_ = encoder.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": frame.RequestID, "response": map[string]any{}}})
			if frame.Request.Subtype == "interrupt" && pending != "" {
				_ = encoder.Encode(map[string]any{"type": "result", "subtype": "success", "session_id": session, "user_message_uuid": pending})
			}
		case "user":
			pending = frame.UUID
			_ = encoder.Encode(json.RawMessage(scanner.Bytes()))
			prompt := ""
			for _, block := range frame.Message.Content {
				prompt += block.Text
			}
			if prompt == "die" {
				os.Exit(17)
			}
			if prompt == "stop" {
				_ = encoder.Encode(map[string]any{"type": "assistant", "session_id": session, "message": map[string]any{"id": pending, "content": []map[string]string{{"type": "text", "text": "started"}}}})
				continue
			}
			_ = encoder.Encode(map[string]any{"type": "assistant", "session_id": session, "message": map[string]any{"id": pending, "model": "sonnet", "content": []map[string]string{{"type": "text", "text": "hello"}}, "usage": map[string]int{"input_tokens": 15}}})
			_ = encoder.Encode(map[string]any{"type": "result", "subtype": "success", "session_id": session, "user_message_uuid": pending, "usage": map[string]int{"input_tokens": 15, "output_tokens": 2}})
			_ = encoder.Encode(map[string]any{"type": "system", "subtype": "session_state_changed", "state": "idle"})
		}
	}
}

func directTestRunner(t *testing.T) *runner {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	r := &runner{deps: agent.Dependencies{Root: root, Context: t.Context(), Env: []string{"REPOGO_FAKE_CLAUDE_RUNNER=1"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, executable: func() string { return executable }, home: root}
	t.Cleanup(r.Close)
	return r
}

func TestDirectEnvironmentPreservesConfigDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name, inherited, root, injected, want string
	}{
		{name: "default"},
		{name: "explicit", inherited: "/custom/config", want: "/custom/config"},
		{name: "injected", injected: "/injected/config", want: "/injected/config"},
		{name: "isolated", inherited: "/custom/config", root: "/test", want: "/test/claude"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CLAUDE_CONFIG_DIR", tc.inherited)
			r := &runner{home: "/test/claude", deps: agent.Dependencies{Root: tc.root}}
			if tc.injected != "" {
				r.deps.Env = []string{"CLAUDE_CONFIG_DIR=" + tc.injected}
			}
			var got string
			for _, entry := range r.environment() {
				if value, ok := strings.CutPrefix(entry, "CLAUDE_CONFIG_DIR="); ok {
					got = value
				}
			}
			if got != tc.want {
				t.Fatalf("config directory = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDirectRunnerReuseAndResume(t *testing.T) {
	r := directTestRunner(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req := agent.TurnRequest{ChatID: "new", Cwd: t.TempDir(), Prompt: "hello", Config: agent.TurnConfig{Model: "sonnet", ReasoningLevel: "high", PermissionMode: agent.PermissionApprovalRequired}}
	var text strings.Builder
	turnIO := agent.TurnIO{TurnID: "turn-1", Emit: func(e agent.Event) { text.WriteString(e.Text) }, Session: func(id string) { req.SessionID = id }}
	first, err := r.Send(ctx, req, turnIO)
	if err != nil {
		t.Fatal(err)
	}
	if text.String() != "hello" || first.SessionID == "" || first.Usage.InputTokens != 15 {
		t.Fatalf("first result %#v, text %q", first, text.String())
	}
	req.ChatID = agent.ChatID(agent.KindClaude, first.SessionID)
	session, _ := r.pool.Get(req.ChatID)
	r.pool.Release(req.ChatID, session)
	second, err := r.Send(ctx, req, turnIO)
	if err != nil || second.SessionID != first.SessionID {
		t.Fatalf("second %#v: %v", second, err)
	}
	reused, _ := r.pool.Get(req.ChatID)
	r.pool.Release(req.ChatID, reused)
	if reused != session {
		t.Fatal("did not reuse process")
	}
	r.Close()
	third, err := r.Send(ctx, req, turnIO)
	if err != nil || third.SessionID != first.SessionID {
		t.Fatalf("resume %#v: %v", third, err)
	}
}

func TestDirectRunnerStopAndProcessDeath(t *testing.T) {
	for _, prompt := range []string{"stop", "die"} {
		t.Run(prompt, func(t *testing.T) {
			r := directTestRunner(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			events := agent.TurnIO{TurnID: "turn", Emit: func(e agent.Event) {
				if e.Text == "started" {
					cancel()
				}
			}}
			result, err := r.Send(ctx, agent.TurnRequest{ChatID: "new", Cwd: t.TempDir(), Prompt: prompt}, events)
			if err == nil {
				t.Fatal("expected stopped/dead error")
			}
			if prompt == "stop" && result.StopReason != "cancelled" {
				t.Fatal(result)
			}
			if prompt == "die" && !strings.Contains(err.Error(), "17") {
				t.Fatal(err)
			}
		})
	}
}

func boundSession(t *testing.T) (*liveSession, *[]agent.Event, <-chan turnOutcome) {
	t.Helper()
	s := newLiveSession("session", t.TempDir())
	events := []agent.Event{}
	s.io = &agent.TurnIO{TurnID: "turn", Emit: func(e agent.Event) { events = append(events, e) }}
	s.outcome = make(chan turnOutcome, 1)
	s.blocks = map[string]map[int]*streamBlock{}
	s.calls = map[string]string{}
	s.seenResults = map[string]bool{}
	s.promptSent = true
	s.promptID = "prompt"
	return s, &events, s.outcome
}

func feed(t *testing.T, s *liveSession, raw string) {
	t.Helper()
	var m claudecode.Message
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	s.handleMessage(m)
}

func TestBackgroundSubagentHoldsUntilFollowup(t *testing.T) {
	s, _, outcome := boundSession(t)
	feed(t, s, `{"type":"system","subtype":"task_started","task_id":"child","subagent_type":"Explore","tool_use_id":"tool"}`)
	feed(t, s, `{"type":"result","subtype":"success"}`)
	select {
	case <-outcome:
		t.Fatal("ended while subagent running")
	default:
	}
	feed(t, s, `{"type":"system","subtype":"task_notification","task_id":"child","status":"completed"}`)
	select {
	case <-outcome:
		t.Fatal("notification ended turn before summary")
	default:
	}
	feed(t, s, `{"type":"assistant","message":{"id":"summary","content":[{"type":"text","text":"summary"}]}}`)
	feed(t, s, `{"type":"result","subtype":"success"}`)
	select {
	case got := <-outcome:
		if got.err != nil {
			t.Fatal(got.err)
		}
	default:
		t.Fatal("followup did not settle")
	}
}

func TestStreamSnapshotDedupAndSubagentFiltering(t *testing.T) {
	s, events, _ := boundSession(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "stream-json-sdk-0.3.284.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		feed(t, s, line)
	}
	var text strings.Builder
	for _, event := range *events {
		text.WriteString(event.Text)
	}
	if text.String() != "hello" {
		t.Fatalf("text duplicated or child leaked: %q", text.String())
	}
}

func TestTrailingIdleDoesNotFailNextPrompt(t *testing.T) {
	s, _, outcome := boundSession(t)
	s.promptEchoed = true
	s.owedIdle = 1
	feed(t, s, `{"type":"system","subtype":"session_state_changed","state":"idle"}`)
	select {
	case <-outcome:
		t.Fatal("trailing idle settled next turn")
	default:
	}
	feed(t, s, `{"type":"result","subtype":"success"}`)
	select {
	case <-outcome:
	default:
		t.Fatal("result did not settle")
	}
}

func TestBackgroundShellRetainsSessionWithoutHoldingTurn(t *testing.T) {
	s, _, outcome := boundSession(t)
	feed(t, s, `{"type":"system","subtype":"task_started","task_id":"shell","task_type":"local_bash","is_backgrounded":true}`)
	feed(t, s, `{"type":"result","subtype":"success"}`)
	select {
	case <-outcome:
	default:
		t.Fatal("shell held the turn")
	}
	s.io = nil
	if !s.busyLocked() {
		t.Fatal("background process is reapable")
	}
	feed(t, s, `{"type":"system","subtype":"task_updated","task_id":"shell","patch":{"status":"completed"}}`)
	if s.busyLocked() {
		t.Fatal("completed shell retained session")
	}
}

func TestBlockSnapshotsCanShareMessageID(t *testing.T) {
	s, events, _ := boundSession(t)
	feed(t, s, `{"type":"assistant","uuid":"a","message":{"id":"same","content":[{"type":"thinking","thinking":"thinking"}]}}`)
	feed(t, s, `{"type":"assistant","uuid":"b","message":{"id":"same","content":[{"type":"text","text":"first"}]}}`)
	feed(t, s, `{"type":"assistant","uuid":"c","message":{"id":"same","content":[{"type":"text","text":"second"}]}}`)
	if len(*events) != 3 || (*events)[1].Text != "first" || (*events)[2].Text != "second" {
		t.Fatal(*events)
	}
}

func TestStreamedToolCallCarriesItsWholeInput(t *testing.T) {
	s, events, _ := boundSession(t)
	feed(t, s, `{"type":"stream_event","event":{"type":"message_start","message":{"id":"m"}}}`)
	feed(t, s, `{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Agent","input":{}}}}`)
	feed(t, s, `{"type":"assistant","uuid":"a","message":{"id":"m","content":[{"type":"tool_use","id":"toolu_1","name":"Agent","input":{"description":"Explore RPC handling","subagent_type":"Explore"}}]}}`)
	if len(*events) != 1 || (*events)[0].Tool == nil {
		t.Fatalf("want one tool call, got %+v", *events)
	}
	if got := string((*events)[0].Tool.Input); got != `{"description":"Explore RPC handling","subagent_type":"Explore"}` {
		t.Fatalf("input = %s", got)
	}
}

func TestAutonomousResultCannotSettleUserTurn(t *testing.T) {
	s, _, outcome := boundSession(t)
	feed(t, s, `{"type":"result","is_error":true,"origin":{"kind":"peer"},"errors":["unrelated error"],"num_turns":1}`)
	select {
	case <-outcome:
		t.Fatal("unrelated result settled user turn")
	default:
	}
	feed(t, s, `{"type":"system","subtype":"task_started","task_id":"child","subagent_type":"Explore"}`)
	feed(t, s, `{"type":"result","subtype":"success"}`)
	feed(t, s, `{"type":"system","subtype":"task_notification","task_id":"child","status":"completed"}`)
	feed(t, s, `{"type":"result","origin":{"kind":"task-notification"},"num_turns":0}`)
	select {
	case <-outcome:
		t.Fatal("placeholder settled before summary")
	default:
	}
	feed(t, s, `{"type":"result","origin":{"kind":"task-notification"},"num_turns":1}`)
	select {
	case result := <-outcome:
		if result.err != nil {
			t.Fatal(result.err)
		}
	default:
		t.Fatal("summary did not settle")
	}
}

func TestDirectRefusesChangedSessionIdentity(t *testing.T) {
	s, _, outcome := boundSession(t)
	feed(t, s, `{"type":"assistant","session_id":"wrong","message":{"content":[]}}`)
	select {
	case result := <-outcome:
		if result.err == nil {
			t.Fatal("accepted another transcript")
		}
	default:
		t.Fatal("identity mismatch ignored")
	}
}

func TestProviderRunsClaudeDirectly(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := New(agent.Dependencies{
		Root: t.TempDir(), Context: t.Context(),
		Env: []string{"REPOGO_FAKE_CLAUDE_RUNNER=1"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	defer p.Close()
	p.runner.executable = func() string { return binary }
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var text strings.Builder
	result, err := p.Send(ctx, agent.TurnRequest{ChatID: "new", Cwd: t.TempDir(), Prompt: "hello"},
		agent.TurnIO{TurnID: "turn", Emit: func(e agent.Event) { text.WriteString(e.Text) }})
	if err != nil {
		t.Fatal(err)
	}
	if result.SessionID == "" || text.String() != "hello" {
		t.Fatalf("result %#v, text %q", result, text.String())
	}
}
