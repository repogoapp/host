package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/codexappserver"
)

func TestMain(m *testing.M) {
	if os.Getenv("REPOGO_FAKE_CODEX") == "1" {
		fakeAppServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type frame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}

// fakeAppServer answers like `codex app-server`, choosing what a turn does
// from its prompt's text.
func fakeAppServer() {
	out := json.NewEncoder(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64<<10), 1<<20)
	send := func(v any) { _ = out.Encode(v) }
	notify := func(method string, params map[string]any) { send(map[string]any{"method": method, "params": params}) }
	thread, turns := "", 0
	// The turn left running for steers, and whether it is planning.
	active, planning := "", false
	// ask sends Codex's own request and reads until the host answers it.
	ask := func(method string, params map[string]any) json.RawMessage {
		send(map[string]any{"id": 900, "method": method, "params": params})
		for in.Scan() {
			var f frame
			if json.Unmarshal(in.Bytes(), &f) == nil && string(f.ID) == "900" && f.Method == "" {
				return f.Result
			}
		}
		os.Exit(13)
		return nil
	}
	for in.Scan() {
		var f frame
		if json.Unmarshal(in.Bytes(), &f) != nil {
			os.Exit(12)
		}
		var p struct {
			ThreadID            string `json:"threadId"`
			TurnID              string `json:"turnId"`
			ExpectedTurnID      string `json:"expectedTurnId"`
			ClientUserMessageID string `json:"clientUserMessageId"`
			CollaborationMode   *struct {
				Mode string `json:"mode"`
			} `json:"collaborationMode"`
			Input []struct {
				Text string `json:"text"`
			} `json:"input"`
		}
		_ = json.Unmarshal(f.Params, &p)
		reply := func(result any) { send(map[string]any{"id": f.ID, "result": result}) }
		switch f.Method {
		case "initialize":
			reply(map[string]any{})
		case "thread/start":
			thread = "thread-new"
			reply(map[string]any{"thread": map[string]any{"id": thread}, "model": "gpt-5", "reasoningEffort": "medium"})
		case "thread/resume":
			if p.ThreadID == "missing" {
				send(map[string]any{"id": f.ID, "error": map[string]any{"code": -32600, "message": "no rollout found"}})
				continue
			}
			thread = p.ThreadID
			reply(map[string]any{"thread": map[string]any{"id": thread}, "model": "gpt-5"})
		case "turn/steer":
			if active == "" || planning {
				send(map[string]any{"id": f.ID, "error": map[string]any{"code": -32600, "message": "no active turn to steer"}})
				continue
			}
			if p.ExpectedTurnID != active || p.ClientUserMessageID == "" {
				send(map[string]any{"id": f.ID, "error": map[string]any{"code": -32600, "message": "expected active turn id"}})
				continue
			}
			reply(map[string]any{"turnId": active})
			notify("item/agentMessage/delta", map[string]any{"threadId": thread, "turnId": active, "itemId": "m1", "delta": "+" + p.Input[0].Text})
			notify("turn/completed", map[string]any{"threadId": thread, "turn": map[string]any{"id": active, "status": "completed"}})
			active = ""
		case "turn/interrupt":
			active = ""
			reply(map[string]any{})
			notify("turn/completed", map[string]any{"threadId": thread, "turn": map[string]any{"id": p.TurnID, "status": "interrupted"}})
		case "turn/start":
			turns++
			turn := "turn-" + string(rune('0'+turns))
			text := ""
			for _, i := range p.Input {
				text += i.Text
			}
			delta := func(thread, s string) {
				notify("item/agentMessage/delta", map[string]any{"threadId": thread, "turnId": turn, "itemId": "m1", "delta": s})
			}
			done := func() {
				notify("turn/completed", map[string]any{"threadId": thread, "turn": map[string]any{"id": turn, "status": "completed"}})
			}
			if text == "fast" {
				delta(thread, "fast")
				done()
				reply(map[string]any{"turn": map[string]any{"id": turn, "status": "inProgress"}})
				continue
			}
			reply(map[string]any{"turn": map[string]any{"id": turn, "status": "inProgress"}})
			switch text {
			case "die":
				os.Exit(17)
			case "stop":
				delta(thread, "started")
				continue
			case "wait":
				active, planning = turn, p.CollaborationMode != nil && p.CollaborationMode.Mode == "plan"
				delta(thread, "started")
				continue
			case "hello":
				delta(thread, "hel")
				delta(thread, "lo")
				notify("thread/tokenUsage/updated", map[string]any{"threadId": thread, "turnId": turn, "tokenUsage": map[string]any{
					"last":  map[string]int{"inputTokens": 20, "cachedInputTokens": 5, "outputTokens": 3, "totalTokens": 23},
					"total": map[string]int{"inputTokens": 20, "cachedInputTokens": 5, "outputTokens": 3, "totalTokens": 23}, "modelContextWindow": 1000}})
			case "subagent":
				delta("child-thread", "theirs")
				delta(thread, "mine")
			case "tool":
				notify("item/started", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"id": "call_1", "type": "commandExecution", "command": "ls", "cwd": "/w", "status": "inProgress"}})
				notify("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"id": "call_1", "type": "commandExecution", "command": "ls", "cwd": "/w", "status": "completed", "aggregatedOutput": "ok", "exitCode": 0}})
				notify("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"id": "m2", "type": "agentMessage", "text": "whole"}})
				notify("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"id": "call_4", "type": "agentMessage", "text": "Which?\n- Yes\n- No", "delivery": "async"}})
			case "approve":
				answer := ask("item/commandExecution/requestApproval", map[string]any{"threadId": thread, "turnId": turn, "itemId": "call_2", "command": "rm -rf build", "cwd": "/w",
					"availableDecisions":          []any{"accept", map[string]any{"acceptWithExecpolicyAmendment": map[string]any{"execpolicy_amendment": []string{"rm"}}}, "acceptForSession", "decline", "cancel"},
					"proposedExecpolicyAmendment": []string{"rm"}})
				delta(thread, string(answer))
			case "ask":
				answer := ask("item/tool/requestUserInput", map[string]any{"threadId": thread, "turnId": turn, "itemId": "call_3", "isBlocking": true,
					"questions": []any{map[string]any{"id": "color", "header": "Color", "question": "Which color?", "isOther": true,
						"options": []any{map[string]string{"label": "Blue", "description": ""}}}}})
				delta(thread, string(answer))
			case "own":
				answer := ask("mcpServer/elicitation/request", map[string]any{"threadId": thread, "serverName": agent.ToolsServer, "mode": "form", "message": "Run browser?", "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{}}})
				delta(thread, string(answer))
			}
			done()
		}
	}
}

func testRunner(t *testing.T) *runner {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	r := &runner{deps: agent.Dependencies{Root: root, Context: t.Context(), Env: []string{"REPOGO_FAKE_CODEX=1"},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, executable: func() string { return executable }, home: root}
	t.Cleanup(r.Close)
	return r
}

// turnOf runs one turn and returns what it said and the events it emitted.
func turnOf(t *testing.T, r *runner, req agent.TurnRequest, ask func(agent.Approval) json.RawMessage) (agent.Result, string, []agent.Event, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var text strings.Builder
	var events []agent.Event
	io := agent.TurnIO{TurnID: "turn", Emit: func(e agent.Event) {
		events = append(events, e)
		text.WriteString(e.Text)
	}, Ask: func(_ context.Context, a agent.Approval) (json.RawMessage, error) {
		return ask(a), nil
	}}
	result, err := r.Send(ctx, req, io)
	return result, text.String(), events, err
}

func TestRunnerStreamsReusesAndResumes(t *testing.T) {
	r := testRunner(t)
	req := agent.TurnRequest{ChatID: "new", Cwd: t.TempDir(), Prompt: "hello"}
	first, text, _, err := turnOf(t, r, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if text != "hello" || first.SessionID != "thread-new" {
		t.Fatalf("first turn %#v said %q", first, text)
	}
	if u := first.Usage; u.InputTokens != 15 || u.CacheReadTokens != 5 || u.OutputTokens != 3 || u.ContextSize != 1000 {
		t.Fatalf("usage %#v", u)
	}
	req.ChatID, req.SessionID = agent.ChatID(agent.KindCodex, first.SessionID), first.SessionID
	session, _ := r.pool.Get(req.ChatID)
	r.pool.Release(req.ChatID, session)
	if _, _, _, err := turnOf(t, r, req, nil); err != nil {
		t.Fatal(err)
	}
	reused, _ := r.pool.Get(req.ChatID)
	r.pool.Release(req.ChatID, reused)
	if reused != session {
		t.Fatal("did not reuse the app server")
	}
	r.Close()
	resumed, _, _, err := turnOf(t, r, req, nil)
	if err != nil || resumed.SessionID != first.SessionID {
		t.Fatalf("resume %#v: %v", resumed, err)
	}
}

func TestResumeFailureIsAnErrorNotAFork(t *testing.T) {
	r := testRunner(t)
	_, _, _, err := turnOf(t, r, agent.TurnRequest{ChatID: "codex:missing", SessionID: "missing", Cwd: t.TempDir(), Prompt: "hello"}, nil)
	if err == nil || !strings.Contains(err.Error(), "reopen Codex chat") {
		t.Fatalf("got %v", err)
	}
}

func TestTurnThatCompletesBeforeStartAnswersSettles(t *testing.T) {
	_, text, _, err := turnOf(t, testRunner(t), agent.TurnRequest{ChatID: "new", Cwd: t.TempDir(), Prompt: "fast"}, nil)
	if err != nil || text != "fast" {
		t.Fatalf("said %q: %v", text, err)
	}
}

func TestSubagentThreadDoesNotSpeakInTheChat(t *testing.T) {
	_, text, _, err := turnOf(t, testRunner(t), agent.TurnRequest{ChatID: "new", Cwd: t.TempDir(), Prompt: "subagent"}, nil)
	if err != nil || text != "mine" {
		t.Fatalf("said %q: %v", text, err)
	}
}

func TestToolCallsCarryTheirRolloutNames(t *testing.T) {
	_, text, events, err := turnOf(t, testRunner(t), agent.TurnRequest{ChatID: "new", Cwd: t.TempDir(), Prompt: "tool"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var call, result *agent.ToolCall
	for _, e := range events {
		switch e.Kind {
		case agent.EventToolCall:
			call = e.Tool
		case agent.EventToolResult:
			result = e.Tool
		}
	}
	if call == nil || call.Name != "exec_command" || call.CallID != "call_1" || !strings.Contains(string(call.Input), `"cmd":"ls"`) {
		t.Fatalf("call %#v", call)
	}
	if result == nil || result.Output != "ok" || result.IsError {
		t.Fatalf("result %#v", result)
	}
	if text != "whole" {
		t.Fatalf("a message that arrived whole said %q; an async question's message says nothing", text)
	}
}

func TestApprovalOffersCodexDecisionsInOrder(t *testing.T) {
	var asked agent.Approval
	_, text, _, err := turnOf(t, testRunner(t), agent.TurnRequest{ChatID: "new", Cwd: t.TempDir(), Prompt: "approve"}, func(a agent.Approval) json.RawMessage {
		asked = a
		return json.RawMessage(`"acceptWithExecpolicyAmendment"`)
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, o := range asked.Options {
		ids = append(ids, o.OptionID)
	}
	want := []string{"accept", "acceptWithExecpolicyAmendment", "acceptForSession", "decline", "cancel"}
	if !slices.Equal(ids, want) || asked.CallID != "call_2" || asked.Kind != "execute" || asked.Title != "rm -rf build" {
		t.Fatalf("asked %#v", asked)
	}
	if text != `{"decision":{"acceptWithExecpolicyAmendment":{"execpolicy_amendment":["rm"]}}}` {
		t.Fatalf("Codex got %s", text)
	}
}

func TestQuestionAnswersGoBackByID(t *testing.T) {
	var asked agent.Approval
	_, text, _, err := turnOf(t, testRunner(t), agent.TurnRequest{ChatID: "new", Cwd: t.TempDir(), Prompt: "ask"}, func(a agent.Approval) json.RawMessage {
		asked = a
		return json.RawMessage(`{"color":"Blue","color_other":" teal "}`)
	})
	if err != nil {
		t.Fatal(err)
	}
	if asked.Kind != agent.ApprovalQuestion || len(asked.Questions) != 1 || asked.Questions[0].CustomID != "color_other" {
		t.Fatalf("asked %#v", asked)
	}
	if text != `{"answers":{"color":{"answers":["Blue","teal"]}}}` {
		t.Fatalf("Codex got %s", text)
	}
}

func TestOwnToolsRunWithoutAsking(t *testing.T) {
	_, text, _, err := turnOf(t, testRunner(t), agent.TurnRequest{ChatID: "new", Cwd: t.TempDir(), Prompt: "own"}, func(agent.Approval) json.RawMessage {
		t.Error("asked about RepoGo's own tool")
		return nil
	})
	if err != nil || text != `{"action":"accept","content":{}}` {
		t.Fatalf("Codex got %s: %v", text, err)
	}
}

func TestStopAndProcessDeath(t *testing.T) {
	for _, prompt := range []string{"stop", "die"} {
		t.Run(prompt, func(t *testing.T) {
			r := testRunner(t)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			io := agent.TurnIO{TurnID: "turn", Emit: func(e agent.Event) {
				if e.Text == "started" {
					cancel()
				}
			}}
			result, err := r.Send(ctx, agent.TurnRequest{ChatID: "new", Cwd: t.TempDir(), Prompt: prompt}, io)
			if err == nil {
				t.Fatal("expected a stopped or dead turn to fail")
			}
			if prompt == "stop" && result.StopReason != "cancelled" {
				t.Fatalf("stopped turn %#v", result)
			}
			if prompt == "die" && !strings.Contains(err.Error(), "17") {
				t.Fatal(err)
			}
		})
	}
}

// A steer lands only in the turn the host is running, and not while it plans.
func TestSteerJoinsTheRunningTurn(t *testing.T) {
	for _, mode := range []string{"default", "plan"} {
		t.Run(mode, func(t *testing.T) {
			r := testRunner(t)
			answer := agent.TurnRequest{ChatID: "codex:thread-new", Prompt: "blue"}
			if err := r.Steer(t.Context(), "turn", answer); err == nil {
				t.Fatal("steered a chat with no session")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			started := make(chan struct{})
			var text strings.Builder
			io := agent.TurnIO{TurnID: "turn", Emit: func(e agent.Event) {
				text.WriteString(e.Text)
				if e.Text == "started" {
					close(started)
				}
			}}
			done := make(chan error, 1)
			go func() {
				_, err := r.Send(ctx, agent.TurnRequest{ChatID: "new", Cwd: t.TempDir(), Prompt: "wait", Config: agent.TurnConfig{Mode: mode}}, io)
				done <- err
			}()
			<-started
			if err := r.Steer(ctx, "another", answer); err == nil {
				t.Fatal("steered a turn the host is not running")
			}
			err := r.Steer(ctx, "turn", answer)
			if mode == "plan" {
				if err == nil {
					t.Fatal("steered a planning turn")
				}
				cancel()
				<-done
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil || text.String() != "started+blue" {
				t.Fatalf("turn said %q: %v", text.String(), err)
			}
		})
	}
}

func TestTurnParamsCarryTheTurnsSettings(t *testing.T) {
	s := &liveSession{id: "t", model: "gpt-5", effort: "medium"}
	plan := s.turnParams(agent.TurnRequest{Prompt: "x", Config: agent.TurnConfig{
		Mode: "plan", PermissionMode: agent.PermissionAutoAcceptEdits, ReasoningLevel: "high", FastMode: true}})
	if plan.CollaborationMode == nil || plan.CollaborationMode.Mode != "plan" || plan.CollaborationMode.Settings.Model != "gpt-5" ||
		*plan.CollaborationMode.Settings.ReasoningEffort != "high" {
		t.Fatalf("plan mode %#v", plan.CollaborationMode)
	}
	if plan.ApprovalPolicy != "on-request" || plan.SandboxPolicy.Type != "workspaceWrite" || plan.ServiceTierForTurn != "fast" || plan.Effort != "high" {
		t.Fatalf("plan turn %#v", plan)
	}
	next := s.turnParams(agent.TurnRequest{Prompt: "x", Config: agent.TurnConfig{Model: "gpt-5-mini", PermissionMode: agent.PermissionFullAccess}})
	if next.CollaborationMode.Mode != "default" || next.CollaborationMode.Settings.Model != "gpt-5-mini" {
		t.Fatalf("a turn after a plan keeps planning: %#v", next.CollaborationMode)
	}
	if next.ApprovalPolicy != "never" || next.SandboxPolicy.Type != "dangerFullAccess" || next.ServiceTierForTurn != "default" {
		t.Fatalf("full-access turn %#v", next)
	}
	if kept := s.turnParams(agent.TurnRequest{Prompt: "x"}); kept.ApprovalPolicy != "" || kept.SandboxPolicy != nil {
		t.Fatalf("an unset permission changed the thread's: %#v", kept)
	}
}

func TestPromptSendsImagesAndMentionsFiles(t *testing.T) {
	input := prompt(agent.TurnRequest{Prompt: "look", Attachments: []agent.Attachment{
		{Name: "shot.png", MimeType: "image/png", Path: "/tmp/shot.png"},
		{Name: "notes.txt", MimeType: "text/plain", Path: "/tmp/notes.txt"},
	}})
	want := []codexappserver.Input{
		{Type: "text", Text: "look"},
		{Type: "localImage", Path: "/tmp/shot.png"},
		{Type: "mention", Name: "notes.txt", Path: "/tmp/notes.txt"},
	}
	if !slices.Equal(input, want) {
		t.Fatalf("input %#v", input)
	}
}

func TestMCPServersBecomeCodexConfig(t *testing.T) {
	config := mcpConfig([]agent.MCPServer{{Type: "http", Name: "linear", URL: "https://mcp.linear.app",
		Headers: []agent.Header{{Name: "Authorization", Value: "Bearer x"}}}})
	got, _ := json.Marshal(config)
	if string(got) != `{"mcp_servers":{"linear":{"http_headers":{"Authorization":"Bearer x"},"url":"https://mcp.linear.app"}}}` {
		t.Fatalf("config %s", got)
	}
	if mcpConfig(nil) != nil {
		t.Fatal("no servers still wrote a table")
	}
}

func TestRunningNamesTheTurnAndItsDevice(t *testing.T) {
	s := &liveSession{key: "codex:t", cwd: "/w"}
	if _, ok := s.Running(); ok {
		t.Fatal("running with no turn bound")
	}
	s.io, s.turnCtx = &agent.TurnIO{TurnID: "turn-1", Device: "phone"}, t.Context()
	running, ok := s.Running()
	if !ok || running.TurnID != "turn-1" || running.ChatID != "codex:t" || running.Device != "phone" || running.Cwd != "/w" {
		t.Fatalf("running %#v", running)
	}
}
