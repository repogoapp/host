package cursor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/cursoragent"
	"github.com/repogo/host/internal/session"
)

const fakeID = "1cad37a4-19d9-4dfd-b53e-d98d76e61023"

func TestMain(m *testing.M) {
	if os.Getenv("REPOGO_CURSOR_FAKE") == "1" {
		fakeCursor()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// The fake keeps the prompt pending until the client answers its server request.
func fakeCursor() {
	out := json.NewEncoder(os.Stdout)
	reply := func(id json.RawMessage, result any) {
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	update := func(value any) {
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": fakeID, "update": value}})
	}
	text := func(value string) {
		update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]string{"type": "text", "text": value}})
	}
	request := func(method string, params any) {
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": "callback-1", "method": method, "params": params})
	}
	type savedTurn struct{ Prompt, Reply string }
	var history []savedTurn
	nativeDir := filepath.Join(os.Getenv("CURSOR_CONFIG_DIR"), "acp-sessions", fakeID)
	nativePath := filepath.Join(nativeDir, "store.db")
	save := func() {
		if err := apphome.WriteJSON(nativePath, history, 0o600); err != nil {
			os.Exit(24)
		}
	}
	var pending json.RawMessage
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var m struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
			Result  json.RawMessage `json:"result"`
		}
		if json.Unmarshal(in.Bytes(), &m) != nil || m.JSONRPC != "2.0" {
			os.Exit(13)
		}
		switch m.Method {
		case "initialize":
			reply(m.ID, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true, "promptCapabilities": map[string]bool{"image": true}}})
		case "session/new", "session/load":
			if m.Method == "session/load" {
				found, err := apphome.ReadJSON(nativePath, &history)
				if err != nil || !found {
					os.Exit(25)
				}
				for _, turn := range history {
					update(map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]string{"type": "text", "text": turn.Prompt}})
					text(turn.Reply)
				}
			} else {
				if err := os.MkdirAll(nativeDir, 0o700); err != nil {
					os.Exit(26)
				}
				var params cursoragent.SessionParams
				_ = json.Unmarshal(m.Params, &params)
				_ = apphome.WriteJSON(filepath.Join(nativeDir, "meta.json"), nativeMeta{SchemaVersion: 1, Cwd: params.Cwd, Title: "Test chat"}, 0o600)
				history = nil
				save()
			}
			reply(m.ID, map[string]any{"sessionId": fakeID, "models": map[string]any{"currentModelId": "auto[]", "availableModels": []map[string]string{{"modelId": "auto[]", "name": "Auto"}}}, "modes": map[string]any{"currentModeId": "agent", "availableModes": []map[string]string{{"id": "agent", "name": "Agent"}}}})
		case "session/set_model", "session/set_mode":
			reply(m.ID, struct{}{})
		case "session/prompt":
			var p struct {
				Prompt []cursoragent.Content `json:"prompt"`
			}
			_ = json.Unmarshal(m.Params, &p)
			command := p.Prompt[0].Text
			history = append(history, savedTurn{Prompt: command})
			save()
			if command == "deny" || command == "cancel" || command == "allow" {
				pending = m.ID
				update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "tool-1", "title": "npm cache clean", "kind": "execute", "status": "pending", "rawInput": map[string]string{"command": "npm cache clean"}})
				request("session/request_permission", cursoragent.Permission{SessionID: fakeID, ToolCall: cursoragent.ToolCall{ToolCallID: "tool-1", Title: "npm cache clean", Kind: "execute"}, Options: []cursoragent.PermissionOption{{OptionID: "allow-once", Name: "Allow once", Kind: "allow_once"}, {OptionID: "reject-once", Name: "Reject", Kind: "reject_once"}}})
			} else if command == "question" {
				pending = m.ID
				request("cursor/ask_question", cursoragent.Questions{ToolCallID: "question-1", Title: "Color", Questions: []cursoragent.Question{{ID: "color", Prompt: "Red or Blue?", Options: []cursoragent.QuestionOption{{ID: "red", Label: "Red"}, {ID: "blue", Label: "Blue"}}}}})
			} else {
				history[len(history)-1].Reply = command
				save()
				text(command)
				reply(m.ID, cursoragent.PromptResult{StopReason: "end_turn"})
			}
		case "session/cancel":
			if pending != nil {
				reply(pending, cursoragent.PromptResult{StopReason: "cancelled"})
				pending = nil
			}
		case "":
			if pending == nil {
				continue
			}
			if strings.Contains(string(m.Result), "selectedOptionIds") {
				if !strings.Contains(string(m.Result), `"blue"`) {
					os.Exit(14)
				}
				history[len(history)-1].Reply = "SELECTED_COLOR=Blue"
				save()
				text("SELECTED_COLOR=Blue")
			} else {
				var response cursoragent.PermissionResponse
				_ = json.Unmarshal(m.Result, &response)
				update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "tool-1", "status": "completed"})
				history[len(history)-1].Reply = response.Outcome.OptionID
				save()
				text(response.Outcome.OptionID)
			}
			reply(pending, cursoragent.PromptResult{StopReason: "end_turn"})
			pending = nil
		default:
			reply(m.ID, struct{}{})
		}
	}
}
func testProvider(t *testing.T) *Provider {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := New(agent.Dependencies{Context: t.Context(), Root: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Env: []string{"REPOGO_CURSOR_FAKE=1"}})
	p.executable = func() string { return binary }
	t.Cleanup(p.Close)
	return p
}
func testRequest(t *testing.T, prompt string) agent.TurnRequest {
	t.Helper()
	return agent.TurnRequest{Agent: agent.KindCursor, Cwd: t.TempDir(), Prompt: prompt}
}
func TestNativeTurnsResumeWithoutReplayedDuplicates(t *testing.T) {
	p := testProvider(t)
	req := testRequest(t, "first")
	var events []agent.Event
	stream := agent.TurnIO{TurnID: "t1", Emit: func(e agent.Event) { events = append(events, e) }}
	first, err := p.Send(t.Context(), req, stream)
	if err != nil {
		t.Fatal(err)
	}
	req.ChatID = agent.ChatID(agent.KindCursor, first.SessionID)
	req.SessionID = first.SessionID
	req.Prompt = "second"
	stream.TurnID = "t2"
	if _, err = p.Send(t.Context(), req, stream); err != nil {
		t.Fatal(err)
	}
	p.Close()
	req.Prompt = "third"
	stream.TurnID = "t3"
	if _, err = p.Send(t.Context(), req, stream); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("events: %+v", events)
	}
	metas, err := p.sessions.List()
	if err != nil || len(metas) != 1 {
		t.Fatalf("metas: %+v %v", metas, err)
	}
	saved, err := session.ReadAll(metas[0], p.sessions)
	if err != nil {
		t.Fatal(err)
	}
	var messages []string
	for _, e := range saved {
		if e.Kind == agent.EventText {
			messages = append(messages, e.Text)
		}
		if e.TurnID == "" {
			t.Fatalf("missing turn: %+v", e)
		}
	}
	if strings.Join(messages, ",") != "first,second,third" {
		t.Fatalf("native history: %v", messages)
	}
}
// Cursor's store keeps no times, so a replay must get the host's back or the
// phone has nothing to show for "Worked for".
func TestReplayKeepsTurnTimes(t *testing.T) {
	p := testProvider(t)
	if _, err := p.Send(t.Context(), testRequest(t, "timed"), agent.TurnIO{TurnID: "t1", Emit: func(agent.Event) {}}); err != nil {
		t.Fatal(err)
	}
	p.Close()
	metas, err := p.sessions.List()
	if err != nil || len(metas) != 1 {
		t.Fatalf("metas: %+v %v", metas, err)
	}
	events, err := p.sessions.Snapshot(metas[0])
	if err != nil {
		t.Fatal(err)
	}
	var started, ended int64
	for _, e := range events {
		switch e.Kind {
		case agent.EventTurnStarted:
			started = e.At
		case agent.EventTurnFinished:
			ended = e.At
		}
	}
	if started == 0 || ended < started {
		t.Fatalf("replayed times started=%d ended=%d: %+v", started, ended, events)
	}
	if err = p.sessions.Delete(metas[0]); err != nil {
		t.Fatal(err)
	}
	if times, err := p.sessions.readTimes(metas[0].ID); err != nil || len(times) != 0 {
		t.Fatalf("times survived delete: %+v %v", times, err)
	}
}
func TestDeniedToolIsNotSuccess(t *testing.T) {
	p := testProvider(t)
	var events []agent.Event
	result, err := p.Send(t.Context(), testRequest(t, "deny"), agent.TurnIO{TurnID: "deny", Emit: func(e agent.Event) { events = append(events, e) }, Ask: func(ctx context.Context, a agent.Approval) (json.RawMessage, error) {
		if a.CallID != "tool-1" || len(a.Options) != 2 || !strings.Contains(string(a.Input), "npm cache clean") {
			t.Errorf("approval: %+v", a)
		}
		return json.RawMessage(`"reject-once"`), nil
	}})
	if err != nil || result.StopReason != "end_turn" {
		t.Fatalf("%+v %v", result, err)
	}
	found := false
	for _, e := range events {
		if e.Kind == agent.EventToolResult {
			found = true
			if !e.Tool.IsError || e.Tool.Output != "Permission denied" {
				t.Errorf("result: %+v", e.Tool)
			}
		}
	}
	if !found {
		t.Fatal("missing tool result")
	}
}
func TestQuestionLabelsBecomeCursorOptionIDs(t *testing.T) {
	p := testProvider(t)
	var text string
	_, err := p.Send(t.Context(), testRequest(t, "question"), agent.TurnIO{TurnID: "question", Emit: func(e agent.Event) { text += e.Text }, Ask: func(ctx context.Context, a agent.Approval) (json.RawMessage, error) {
		if a.Kind != agent.ApprovalQuestion || a.Questions[0].Options[1].Label != "Blue" {
			t.Errorf("question: %+v", a)
		}
		return json.RawMessage(`{"color":"Blue"}`), nil
	}})
	if err != nil || text != "SELECTED_COLOR=Blue" {
		t.Fatalf("%q %v", text, err)
	}
}
func TestCancelWhileApprovalWaits(t *testing.T) {
	p := testProvider(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready := make(chan struct{})
	done := make(chan error, 1)
	var once sync.Once
	req := testRequest(t, "cancel")
	go func() {
		_, err := p.Send(ctx, req, agent.TurnIO{TurnID: "cancel", Emit: func(agent.Event) {}, Ask: func(ctx context.Context, a agent.Approval) (json.RawMessage, error) {
			once.Do(func() { close(ready) })
			<-ctx.Done()
			return nil, ctx.Err()
		}})
		done <- err
	}()
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("no approval")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancel hung")
	}
}
func TestFullAccessOnlyUsesAllowOnce(t *testing.T) {
	p := testProvider(t)
	req := testRequest(t, "allow")
	req.Config.PermissionMode = agent.PermissionFullAccess
	var text string
	_, err := p.Send(t.Context(), req, agent.TurnIO{TurnID: "allow", Emit: func(e agent.Event) { text += e.Text }, Ask: func(context.Context, agent.Approval) (json.RawMessage, error) {
		t.Error("unexpected approval")
		return nil, errors.New("unexpected")
	}})
	if err != nil || text != "allow-once" {
		t.Fatalf("%q %v", text, err)
	}
}
func TestResumeRefusesMissingOrForeignSession(t *testing.T) {
	p := testProvider(t)
	req := testRequest(t, "hello")
	req.SessionID = fakeID
	if _, err := p.Send(t.Context(), req, agent.TurnIO{}); err == nil {
		t.Fatal("resumed missing native session")
	}
	dir, err := p.sessions.directory(fakeID)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = apphome.WriteJSON(filepath.Join(dir, "meta.json"), nativeMeta{SchemaVersion: 1, Cwd: filepath.Join(req.Cwd, "other")}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "store.db"), []byte("[]"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Send(t.Context(), req, agent.TurnIO{}); err == nil {
		t.Fatal("resumed another project")
	}
}

func TestPlanApprovalUsesExistingApprovalContract(t *testing.T) {
	s := &liveSession{turnCtx: t.Context(), io: &agent.TurnIO{Ask: func(ctx context.Context, a agent.Approval) (json.RawMessage, error) {
		if a.Kind != "plan" || a.Title != "Test plan" || len(a.Options) != 2 {
			t.Errorf("plan: %+v", a)
		}
		return json.RawMessage(`"accepted"`), nil
	}}}
	raw := json.RawMessage(`{"toolCallId":"plan-1","name":"Test plan","plan":"Do one thing"}`)
	response, err := s.respond(t.Context(), cursoragent.Request{Method: "cursor/create_plan", Params: raw})
	if err != nil {
		t.Fatal(err)
	}
	if response.(cursoragent.PlanResponse).Outcome.Outcome != "accepted" {
		t.Fatalf("%+v", response)
	}
}
