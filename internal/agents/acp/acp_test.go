package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
)

func TestMain(m *testing.M) {
	if os.Getenv("REPOGO_FAKE_ACP") == "1" {
		fakeAgent()
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

// fakeAgent answers like an ACP agent over stdio, choosing what a prompt does
// from its text. REPOGO_FAKE_ACP_LOAD=1 makes it able to reopen sessions.
func fakeAgent() {
	out := json.NewEncoder(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64<<10), 1<<20)
	send := func(v any) { _ = out.Encode(v) }
	reply := func(id json.RawMessage, result any) {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	// session is the one the prompt being answered names. With
	// REPOGO_FAKE_ACP_MODELS=1 each session offers a model option.
	session, sessions := "", 0
	models := map[string]string{}
	options := func(id string) []any {
		if os.Getenv("REPOGO_FAKE_ACP_MODELS") != "1" {
			return nil
		}
		return []any{map[string]any{"id": "model", "name": "Model", "category": "model", "type": "select",
			"currentValue": models[id], "options": []any{
				map[string]string{"value": "fast", "name": "Fast"},
				map[string]any{"group": "more", "name": "More", "options": []any{map[string]string{"value": "smart", "name": "Smart"}}},
			}}}
	}
	update := func(u map[string]any) {
		send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": session, "update": u}})
	}
	chunk := func(text string) {
		update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}})
	}
	// next reads until the host's reply to id, or a notification named method.
	next := func(id, method string) frame {
		for in.Scan() {
			var f frame
			if json.Unmarshal(in.Bytes(), &f) == nil && ((id != "" && string(f.ID) == id && f.Method == "") || (method != "" && f.Method == method)) {
				return f
			}
		}
		os.Exit(13)
		return frame{}
	}
	for in.Scan() {
		var f frame
		if json.Unmarshal(in.Bytes(), &f) != nil {
			continue
		}
		switch f.Method {
		case "initialize":
			reply(f.ID, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": os.Getenv("REPOGO_FAKE_ACP_LOAD") == "1"}})
		case "session/new":
			sessions++
			id := fmt.Sprintf("s%d", sessions)
			models[id] = "fast"
			reply(f.ID, map[string]any{"sessionId": id, "configOptions": options(id)})
		case "session/set_config_option":
			var p setConfigParams
			_ = json.Unmarshal(f.Params, &p)
			models[p.SessionID] = p.Value
			reply(f.ID, map[string]any{"configOptions": options(p.SessionID)})
		case "session/load":
			var p sessionParams
			_ = json.Unmarshal(f.Params, &p)
			session = p.SessionID
			chunk("replayed history")
			reply(f.ID, map[string]any{})
		case "session/prompt":
			var p promptParams
			_ = json.Unmarshal(f.Params, &p)
			session = p.SessionID
			full := p.Prompt[0].Text
			// A restarted session's prompt leads with the conversation so far;
			// the command is its last paragraph, and an unknown one echoes it all.
			text := full[strings.LastIndex(full, "\n\n")+1:]
			text = strings.TrimSpace(text)
			// Like eve, it logs to stdout between protocol lines.
			_, _ = os.Stdout.WriteString("fake: thinking\n")
			switch {
			case text == "hello":
				chunk("Hel")
				chunk("lo")
				update(map[string]any{"sessionUpdate": "usage_update", "used": 10, "size": 100})
			case text == "tool":
				update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "Roll dice", "kind": "execute", "status": "pending"})
				send(map[string]any{"jsonrpc": "2.0", "id": 900, "method": "session/request_permission", "params": map[string]any{
					"sessionId": session, "toolCall": map[string]any{"toolCallId": "t1", "rawInput": map[string]any{"count": 2}},
					"options": []any{
						map[string]string{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
						map[string]string{"optionId": "no", "name": "Deny", "kind": "reject_once"},
					}}})
				answer := next("900", "")
				var r permissionResult
				_ = json.Unmarshal(answer.Result, &r)
				if r.Outcome.OptionID == "yes" {
					update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "completed",
						"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "4"}}}})
				}
				chunk("rolled")
			case text == "question" || text == "free text":
				update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "ask1", "title": "Ask question", "status": "pending", "rawInput": map[string]any{"question": "Which fruit?"}})
				field := map[string]any{"type": "string", "title": "Which fruit?", "oneOf": []any{
					map[string]string{"const": "a", "title": "Apple"}, map[string]string{"const": "b", "title": "Banana"}}}
				if text == "free text" {
					field = map[string]any{"type": "string", "title": "Name a fruit", "minLength": 1}
				}
				send(map[string]any{"jsonrpc": "2.0", "id": 901, "method": "elicitation/create", "params": map[string]any{
					"mode": "form", "sessionId": session, "message": "A question", "requestedSchema": map[string]any{
						"type": "object", "properties": map[string]any{"answer": field}, "required": []string{"answer"}}}})
				var r elicitResult
				_ = json.Unmarshal(next("901", "").Result, &r)
				chunk(fmt.Sprintf("%s %v", r.Action, r.Content["answer"]))
			case text == "pid":
				chunk(fmt.Sprint(os.Getpid()))
			case text == "model":
				chunk(models[session] + os.Getenv("FAKE_MODEL"))
			case text == "wait":
				chunk("waiting")
				next("", "session/cancel")
				reply(f.ID, map[string]any{"stopReason": "cancelled"})
				continue
			default:
				chunk(full)
			}
			reply(f.ID, map[string]any{"stopReason": "end_turn"})
		}
	}
}

// project is a checkout holding the fake agent, run from its own folder.
func project(t *testing.T) string {
	t.Helper()
	return projectWith(t, nil)
}

// projectWith is project with more agent.json fields.
func projectWith(t *testing.T, fields map[string]any) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	agentDir := filepath.Join(dir, ".repogo", "agents", "fake")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	m := map[string]any{"name": "Fake", "command": executable, "cwd": "agent"}
	for k, v := range fields {
		m[k] = v
	}
	manifest, _ := json.Marshal(m)
	if err := os.WriteFile(filepath.Join(agentDir, "agent.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func provider(t *testing.T, root string, env ...string) *Provider {
	t.Helper()
	p := New(agent.Dependencies{Root: root, Context: t.Context(), Env: append([]string{"REPOGO_FAKE_ACP=1"}, env...),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(p.Close)
	return p
}

// turn is what one Send said: its streamed text, events and the new chat's id.
type turn struct {
	result agent.Result
	err    error
	text   string
	events []agent.Event
	chat   string
	asked  []agent.Approval
}

// send runs one turn, answering each question with answer(approval). A
// "wait" prompt is stopped as soon as the agent starts answering.
func send(t *testing.T, p *Provider, req agent.TurnRequest, answer func(agent.Approval) string) turn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var mu sync.Mutex
	var out turn
	var text strings.Builder
	stream := agent.TurnIO{TurnID: "turn-" + req.Prompt,
		Emit: func(e agent.Event) {
			mu.Lock()
			defer mu.Unlock()
			out.events = append(out.events, e)
			if e.Kind == agent.EventText {
				text.WriteString(e.Text)
				if req.Prompt == "wait" {
					cancel()
				}
			}
		},
		Session: func(id string) { out.chat = id },
		Ask: func(_ context.Context, a agent.Approval) (json.RawMessage, error) {
			mu.Lock()
			out.asked = append(out.asked, a)
			mu.Unlock()
			raw, _ := json.Marshal(answer(a))
			return raw, nil
		},
	}
	out.result, out.err = p.Send(ctx, req, stream)
	out.text = text.String()
	return out
}

// sendRaw runs a turn that answers its one question with raw JSON.
func sendRaw(t *testing.T, p *Provider, req agent.TurnRequest, reply json.RawMessage) turn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	var out turn
	var text strings.Builder
	var mu sync.Mutex
	stream := agent.TurnIO{TurnID: "turn-raw-" + req.Prompt,
		Emit: func(e agent.Event) {
			mu.Lock()
			defer mu.Unlock()
			if e.Kind == agent.EventText {
				text.WriteString(e.Text)
			}
		},
		Session: func(id string) { out.chat = id },
		Ask:     func(context.Context, agent.Approval) (json.RawMessage, error) { return reply, nil },
	}
	out.result, out.err = p.Send(ctx, req, stream)
	out.text = text.String()
	return out
}

func allowAll(a agent.Approval) string {
	return a.Options[0].OptionID
}

// history is the chat as the session store reads it back.
func history(t *testing.T, p *Provider, id string) []agent.Event {
	t.Helper()
	metas, err := p.Sessions().List()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range metas {
		if m.ID == id {
			events, err := session.ReadAll(m, p.Sessions())
			if err != nil {
				t.Fatal(err)
			}
			return events
		}
	}
	t.Fatalf("chat %s not listed in %+v", id, metas)
	return nil
}

func kinds(events []agent.Event) []string {
	var out []string
	for _, e := range events {
		out = append(out, string(e.Kind)+":"+e.Text)
	}
	return out
}

func TestNewChatStreamsAndKeepsItsTranscript(t *testing.T) {
	dir, root := project(t), t.TempDir()
	p := provider(t, root)
	first := send(t, p, agent.TurnRequest{Cwd: dir, Agent: agent.KindACP, ProjectAgent: "fake", Prompt: "hello"}, allowAll)
	if first.err != nil {
		t.Fatal(first.err)
	}
	if first.chat == "" || first.result.SessionID != first.chat {
		t.Fatalf("chat id %q, result %q", first.chat, first.result.SessionID)
	}
	if first.text != "Hello" || first.result.Usage.ContextUsed != 10 || first.result.Usage.ContextSize != 100 {
		t.Fatalf("text %q usage %+v", first.text, first.result.Usage)
	}
	// Picking the agent is the go-ahead: nothing is asked before it runs.
	if len(first.asked) != 0 {
		t.Fatalf("asked %+v", first.asked)
	}
	got := strings.Join(kinds(history(t, p, first.chat)), " | ")
	if got != "turn_started: | user_message:hello | text:Hello | turn_finished:" {
		t.Fatalf("history %s", got)
	}

	// A second chat on a host started afresh is a chat of its own.
	again := provider(t, root)
	second := send(t, again, agent.TurnRequest{Cwd: dir, Agent: agent.KindACP, ProjectAgent: "fake", Prompt: "hello"}, allowAll)
	if second.err != nil || len(second.asked) != 0 || second.chat == first.chat {
		t.Fatalf("second chat: err %v asked %+v", second.err, second.asked)
	}
}

// An edited agent.json may run another command, so the agent restarts with it.
func TestChangedManifestRestartsTheAgent(t *testing.T) {
	dir := project(t)
	p := provider(t, t.TempDir())
	before := send(t, p, agent.TurnRequest{Cwd: dir, ProjectAgent: "fake", Prompt: "pid"}, allowAll)
	manifest := filepath.Join(dir, ".repogo", "agents", "fake", "agent.json")
	var m map[string]any
	raw, _ := os.ReadFile(manifest)
	_ = json.Unmarshal(raw, &m)
	m["name"] = "Changed"
	raw, _ = json.Marshal(m)
	if err := os.WriteFile(manifest, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	after := send(t, p, agent.TurnRequest{Cwd: dir, SessionID: before.chat, Prompt: "pid"}, allowAll)
	if before.err != nil || after.err != nil || before.text == "" || strings.HasSuffix(after.text, before.text) {
		t.Fatalf("before %q (%v), after %q (%v)", before.text, before.err, after.text, after.err)
	}
}

func TestToolCallAsksWithTheAgentsOptions(t *testing.T) {
	dir := project(t)
	p := provider(t, t.TempDir())
	got := send(t, p, agent.TurnRequest{Cwd: dir, ProjectAgent: "fake", Prompt: "tool"}, allowAll)
	if got.err != nil {
		t.Fatal(got.err)
	}
	if len(got.asked) != 1 {
		t.Fatalf("asked %+v", got.asked)
	}
	tool := got.asked[0]
	if tool.CallID != "t1" || tool.Title != "Roll dice" || tool.Kind != "execute" || string(tool.Input) != `{"count":2}` ||
		len(tool.Options) != 2 || tool.Options[0].Kind != "allow_once" {
		t.Fatalf("tool approval %+v", tool)
	}
	var call, result *agent.ToolCall
	for _, e := range history(t, p, got.chat) {
		switch e.Kind {
		case agent.EventToolCall:
			call = e.Tool
		case agent.EventToolResult:
			result = e.Tool
		}
	}
	if call == nil || call.Name != "Roll dice" || string(call.Input) != `{"count":2}` {
		t.Fatalf("call %+v", call)
	}
	if result == nil || result.CallID != "t1" || result.Output != "4" || result.IsError {
		t.Fatalf("result %+v", result)
	}
}

func TestStopCancelsTheAgentsTurn(t *testing.T) {
	dir := project(t)
	p := provider(t, t.TempDir())
	got := send(t, p, agent.TurnRequest{Cwd: dir, ProjectAgent: "fake", Prompt: "wait"}, allowAll)
	if !errors.Is(got.err, context.Canceled) || got.result.StopReason != stopCancelled {
		t.Fatalf("err %v stop %q", got.err, got.result.StopReason)
	}
	// The process confirmed the cancel, so it is kept for the next message.
	next := send(t, p, agent.TurnRequest{Cwd: dir, SessionID: got.chat, ProjectAgent: "fake", Prompt: "hello"}, allowAll)
	if next.err != nil || next.text != "Hello" {
		t.Fatalf("after stop: %v %q", next.err, next.text)
	}
}

func TestRestartedAgentGetsTheConversationOrReopensIt(t *testing.T) {
	for _, load := range []bool{false, true} {
		dir, root := project(t), t.TempDir()
		env := []string{}
		if load {
			env = append(env, "REPOGO_FAKE_ACP_LOAD=1")
		}
		first := send(t, provider(t, root, env...), agent.TurnRequest{Cwd: dir, ProjectAgent: "fake", Prompt: "hello"}, allowAll)
		if first.err != nil {
			t.Fatal(first.err)
		}
		// A new host has no process for the chat and starts one.
		later := send(t, provider(t, root, env...), agent.TurnRequest{Cwd: dir, SessionID: first.chat, Prompt: "echo"}, allowAll)
		if later.err != nil {
			t.Fatal(later.err)
		}
		if load {
			if later.text != "echo" {
				t.Fatalf("reopened session got %q", later.text)
			}
			continue
		}
		if !strings.Contains(later.text, "User: hello") || !strings.Contains(later.text, "You: Hello") ||
			strings.Contains(later.text, "User: echo") || !strings.HasSuffix(later.text, "echo") {
			t.Fatalf("new session got %q", later.text)
		}
	}
}

// A form the agent sends is the phone's question card: choices by their
// titles, read back as the values the agent wrote.
func TestQuestionsAreAskedAndAnsweredWithTheAgentsValues(t *testing.T) {
	dir := project(t)
	p := provider(t, t.TempDir())
	// The first turn only records what was asked; its answer isn't a form's.
	var asked agent.Approval
	got := send(t, p, agent.TurnRequest{Cwd: dir, ProjectAgent: "fake", Prompt: "question"}, func(a agent.Approval) string {
		asked = a
		return ""
	})
	if asked.Kind != agent.ApprovalQuestion || asked.CallID != "ask1" || len(asked.Questions) != 1 || asked.Questions[0].Text != "Which fruit?" ||
		len(asked.Questions[0].Options) != 2 || asked.Questions[0].Options[1].Label != "Banana" {
		t.Fatalf("asked %+v", asked)
	}
	// The phone answers a question with an object keyed by its id.
	for prompt, reply := range map[string]string{"question": `{"answer":"Banana"}`, "free text": `{"answer":"Kiwi"}`} {
		res := sendRaw(t, p, agent.TurnRequest{Cwd: dir, SessionID: got.chat, Prompt: prompt}, json.RawMessage(reply))
		want := map[string]string{"question": "accept b", "free text": "accept Kiwi"}[prompt]
		if res.err != nil || res.text != want {
			t.Fatalf("%s: %v %q, want %q", prompt, res.err, res.text, want)
		}
	}
}

// Every turn names its chat, so the phone follows each turn live.
func TestEveryTurnReportsItsChat(t *testing.T) {
	dir := project(t)
	p := provider(t, t.TempDir())
	first := send(t, p, agent.TurnRequest{Cwd: dir, ProjectAgent: "fake", Prompt: "hello"}, allowAll)
	next := send(t, p, agent.TurnRequest{Cwd: dir, SessionID: first.chat, Prompt: "hello"}, allowAll)
	if next.err != nil || next.chat != first.chat {
		t.Fatalf("follow-up turn reported %q, want %q (%v)", next.chat, first.chat, next.err)
	}
}

// One process serves every chat with an agent: eve, for one, refuses a
// second instance in its folder.
func TestChatsWithOneAgentShareItsProcess(t *testing.T) {
	dir := project(t)
	p := provider(t, t.TempDir())
	first := send(t, p, agent.TurnRequest{Cwd: dir, ProjectAgent: "fake", Prompt: "pid"}, allowAll)
	second := send(t, p, agent.TurnRequest{Cwd: dir, ProjectAgent: "fake", Prompt: "pid"}, allowAll)
	if first.err != nil || second.err != nil {
		t.Fatal(first.err, second.err)
	}
	if first.chat == second.chat || first.text == "" || first.text != second.text {
		t.Fatalf("chats %s %s ran in processes %q and %q", first.chat, second.chat, first.text, second.text)
	}
	// Each keeps its own session: the first chat's next turn is still its own.
	again := send(t, p, agent.TurnRequest{Cwd: dir, SessionID: first.chat, Prompt: "hello"}, allowAll)
	if again.err != nil || again.text != "Hello" {
		t.Fatalf("first chat again: %v %q", again.err, again.text)
	}
}

// An agent that offers models over ACP is set per chat, and the picker lists
// what it reported once it ran.
func TestACPModelsAreSetPerChatAndListed(t *testing.T) {
	dir := project(t)
	p := provider(t, t.TempDir(), "REPOGO_FAKE_ACP_MODELS=1")
	if agents, _ := p.ProjectAgents(dir); len(agents) != 1 || len(agents[0].Models) != 0 {
		t.Fatalf("models before a run: %+v", agents)
	}
	first := send(t, p, agent.TurnRequest{Cwd: dir, ProjectAgent: "fake", Prompt: "model"}, allowAll)
	if first.err != nil || first.text != "fast" {
		t.Fatalf("default model: %v %q", first.err, first.text)
	}
	agents, err := p.ProjectAgents(dir)
	if err != nil || len(agents[0].Models) != 2 || agents[0].Models[1].Name != "Smart" || agents[0].Model != "fast" {
		t.Fatalf("listed %+v %v", agents, err)
	}
	smart := send(t, p, agent.TurnRequest{Cwd: dir, SessionID: first.chat, Prompt: "model", Config: agent.TurnConfig{Model: "smart"}}, allowAll)
	if smart.err != nil || smart.text != "smart" {
		t.Fatalf("picked model: %v %q", smart.err, smart.text)
	}
	// Another chat on the same process keeps its own.
	other := send(t, p, agent.TurnRequest{Cwd: dir, ProjectAgent: "fake", Prompt: "model"}, allowAll)
	if other.err != nil || other.text != "fast" {
		t.Fatalf("other chat: %v %q", other.err, other.text)
	}
	if bad := send(t, p, agent.TurnRequest{Cwd: dir, SessionID: first.chat, Prompt: "model", Config: agent.TurnConfig{Model: "huge"}}, allowAll); bad.err == nil {
		t.Fatal("a model the agent doesn't offer was accepted")
	}
}

// A manifest's models reach the agent in its modelEnv; another model
// restarts the agent with it.
func TestManifestModelStartsTheAgentWithIt(t *testing.T) {
	dir := projectWith(t, map[string]any{"modelEnv": "FAKE_MODEL",
		"models": []any{map[string]string{"id": "sonnet", "name": "Sonnet"}, map[string]string{"id": "haiku", "name": "Haiku"}}})
	p := provider(t, t.TempDir())
	agents, err := p.ProjectAgents(dir)
	if err != nil || len(agents[0].Models) != 2 || agents[0].Model != "sonnet" {
		t.Fatalf("listed %+v %v", agents, err)
	}
	first := send(t, p, agent.TurnRequest{Cwd: dir, ProjectAgent: "fake", Prompt: "model"}, allowAll)
	if first.err != nil || first.text != "fastsonnet" {
		t.Fatalf("default: %v %q", first.err, first.text)
	}
	haiku := send(t, p, agent.TurnRequest{Cwd: dir, SessionID: first.chat, Prompt: "model", Config: agent.TurnConfig{Model: "haiku"}}, allowAll)
	if haiku.err != nil || haiku.text != "fasthaiku" {
		t.Fatalf("haiku: %v %q", haiku.err, haiku.text)
	}
}

func TestRenameKeepsTheNewestTitle(t *testing.T) {
	dir := project(t)
	p := provider(t, t.TempDir())
	got := send(t, p, agent.TurnRequest{Cwd: dir, ProjectAgent: "fake", Prompt: "hello"}, allowAll)
	metas, _ := p.Sessions().List()
	for _, title := range []string{"First", "Second"} {
		if err := p.sessions.Rename(metas[0], title); err != nil {
			t.Fatal(err)
		}
	}
	metas, _ = p.Sessions().List()
	if len(metas) != 1 || metas[0].ID != got.chat || metas[0].Title != "Second" || metas[0].Cwd != dir {
		t.Fatalf("metas %+v", metas)
	}
}
