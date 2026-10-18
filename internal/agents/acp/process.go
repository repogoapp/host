package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/projectagent"
	"github.com/repogoapp/host/internal/stdiorpc"
)

// Starting an agent can install it first (npx) or boot a dev server (eve).
const openTimeout = 2 * time.Minute

// cancelGrace is how long a stopped turn waits for the agent to confirm
// before its process is ended instead.
const cancelGrace = 10 * time.Second

// historyLimit caps the earlier conversation handed to an agent that can't
// reopen its own session, from the newest end.
const historyLimit = 24 << 10

// toolOutputLimit caps a tool result kept in the transcript.
const toolOutputLimit = 64 << 10

// agentProcess is one running agent, shared by every chat that runs it: an
// ACP agent serves many sessions, and some (eve) allow one instance per folder.
type agentProcess struct {
	key         string
	fingerprint string
	name        string
	workdir     string
	client      *stdiorpc.Client
	caps        agentCapabilities
	// model is the manifest model the process was started with, through
	// its modelEnv; "" for an agent that sets its model per session.
	model string
	// report keeps the models an agent says its sessions offer.
	report func(agentModels)

	mu sync.Mutex
	// chats is each chat's session by chat key; bySession the same sessions
	// by the agent's id, which its updates and requests name.
	chats     map[string]*chatSession
	bySession map[string]*chatSession
	closed    bool
	lastUsed  time.Time
}

// chatSession is one chat's session in its agent's process.
type chatSession struct {
	id   string
	file *transcript
	turn *turnState
	// loading is set while session/load replays history the file already has.
	loading bool
	// replay hands the chat so far to the first prompt of a session that
	// could not be reopened.
	replay bool
	// models is the session's model option as the agent last reported it.
	models agentModels
}

// start runs the agent and agrees on the protocol; sessions open per chat.
// model is the manifest model it runs, handed over in its modelEnv.
func (p *Provider) start(ctx context.Context, m projectagent.Manifest, model string) (*agentProcess, error) {
	proc := &agentProcess{
		key: m.Key(), fingerprint: m.Fingerprint, name: m.Name, workdir: m.Workdir(), model: model,
		chats: map[string]*chatSession{}, bySession: map[string]*chatSession{}, lastUsed: time.Now(),
		report: func(models agentModels) { _ = p.reported.put(m.Fingerprint, models) },
	}
	env := append(agent.ChildEnv(), p.deps.Env...)
	if model != "" {
		env = append(env, m.ModelEnv+"="+model)
	}
	client, err := stdiorpc.Start(p.deps.Context, stdiorpc.Options{
		Executable: executable(m), Args: m.Args, Cwd: m.Workdir(), Env: env, Version: "2.0", LogLines: true,
	}, stdiorpc.Handlers{Request: proc.request, Notification: proc.notify})
	if err != nil {
		p.deps.Log.Info("ACP agent failed", "agent", m.ID, "step", "start", "error", err)
		return nil, failure{name: m.Name, step: "couldn't start", err: err}
	}
	proc.client = client
	open, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	var init initializeResult
	err = client.Call(open, methodInitialize, initializeParams{
		ProtocolVersion: protocolVersion, ClientInfo: implementation{Name: "repogo"},
	}, &init)
	if err != nil {
		p.deps.Log.Info("ACP agent failed", "agent", m.ID, "step", "initialize", "error", detail(err, client))
		proc.close()
		return nil, failure{name: m.Name, step: "couldn't start", err: err}
	}
	proc.caps = init.AgentCapabilities
	return proc, nil
}

// executable is the manifest's command; a relative path is the agent's own file.
func executable(m projectagent.Manifest) string {
	if strings.HasPrefix(m.Command, "./") || strings.HasPrefix(m.Command, "../") {
		return filepath.Join(m.Dir, m.Command)
	}
	return m.Command
}

// session is the chat's session in this process, opening one when it has
// none: the chat's own again when the agent can load it, else a new one that
// is told the conversation so far.
func (proc *agentProcess) session(ctx context.Context, c chat, servers []mcpServer) (*chatSession, error) {
	proc.mu.Lock()
	cs := proc.chats[c.key()]
	proc.mu.Unlock()
	if cs != nil {
		return cs, nil
	}
	open, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	params := sessionParams{Cwd: proc.workdir, MCPServers: servers}
	if c.session != "" && proc.caps.LoadSession {
		cs = &chatSession{id: c.session, file: c.file, loading: true}
		proc.register(c, cs)
		params.SessionID = c.session
		var loaded sessionResult
		err := proc.client.Call(open, methodLoad, params, &loaded)
		proc.mu.Lock()
		cs.loading = false
		proc.mu.Unlock()
		if err == nil {
			proc.setModels(cs, loaded.ConfigOptions)
			return cs, nil
		}
		proc.forget(c, cs)
		params.SessionID = ""
	}
	var opened newSessionResult
	if err := proc.client.Call(open, methodNewSession, params, &opened); err != nil {
		return nil, failure{name: proc.name, step: "couldn't open a chat", err: err}
	}
	cs = &chatSession{id: opened.SessionID, file: c.file, replay: c.session != "" || c.hasHistory()}
	if err := c.file.append(line{Type: lineSession, Session: cs.id}); err != nil {
		return nil, err
	}
	proc.register(c, cs)
	proc.setModels(cs, opened.ConfigOptions)
	return cs, nil
}

// setModels takes a session's options as the agent reported them, keeping
// its model option for the picker.
func (proc *agentProcess) setModels(cs *chatSession, options []configOption) {
	models, ok := modelOption(options)
	if !ok {
		return
	}
	proc.mu.Lock()
	cs.models = models
	proc.mu.Unlock()
	proc.report(models)
}

// useModel switches a session to the model a turn asks for, when the agent
// offers it per session; a manifest model was set when the process started.
func (proc *agentProcess) useModel(ctx context.Context, cs *chatSession, model string) error {
	proc.mu.Lock()
	current := cs.models
	proc.mu.Unlock()
	if model == "" || proc.model != "" || current.ConfigID == "" || model == current.Current {
		return nil
	}
	if !slices.ContainsFunc(current.Models, func(m projectagent.ProjectAgentModel) bool { return m.ID == model }) {
		return fmt.Errorf("%s doesn't offer %s", proc.name, model)
	}
	var set sessionResult
	err := proc.client.Call(ctx, methodSetConfig, setConfigParams{SessionID: cs.id, ConfigID: current.ConfigID, Value: model}, &set)
	if err != nil {
		return failure{name: proc.name, step: "couldn't switch to " + model, err: err}
	}
	proc.setModels(cs, set.ConfigOptions)
	return nil
}

// modelInUse is the model a session's next turn runs on and its display
// name: the manifest's for the process, else the session's own option.
func (proc *agentProcess) modelInUse(cs *chatSession, m projectagent.Manifest) (id, name string) {
	models := m.Models
	id = proc.model
	if id == "" {
		proc.mu.Lock()
		id, models = cs.models.Current, cs.models.Models
		proc.mu.Unlock()
	}
	for _, model := range models {
		if model.ID == id {
			return id, model.Name
		}
	}
	return id, ""
}

// busy reports whether any chat's turn is running on this process.
func (proc *agentProcess) busy() bool {
	proc.mu.Lock()
	defer proc.mu.Unlock()
	for _, cs := range proc.chats {
		if cs.turn != nil {
			return true
		}
	}
	return false
}

func (proc *agentProcess) register(c chat, cs *chatSession) {
	proc.mu.Lock()
	defer proc.mu.Unlock()
	proc.chats[c.key()], proc.bySession[cs.id] = cs, cs
}

func (proc *agentProcess) forget(c chat, cs *chatSession) {
	proc.mu.Lock()
	defer proc.mu.Unlock()
	delete(proc.chats, c.key())
	delete(proc.bySession, cs.id)
}

// mcpServers is RepoGo's servers for the project, for an agent that takes
// HTTP servers; ACP requires the list, so it is empty rather than absent.
func (p *Provider) mcpServers(ctx context.Context, c chat, caps agentCapabilities) []mcpServer {
	out := []mcpServer{}
	if !caps.MCPCapabilities.HTTP || p.deps.MCP == nil {
		return out
	}
	for _, server := range p.deps.MCP(ctx, c.manifest.Project) {
		headers := []header{}
		for _, h := range server.Headers {
			headers = append(headers, header{Name: h.Name, Value: h.Value})
		}
		out = append(out, mcpServer{Type: "http", Name: server.Name, URL: server.URL, Headers: headers})
	}
	return out
}

// hasHistory reports whether the chat had turns before this one started.
func (c chat) hasHistory() bool {
	events, _ := readEvents(c.file.path)
	for _, e := range events {
		if e.Kind == agent.EventTurnFinished || e.Kind == agent.EventTurnFailed {
			return true
		}
	}
	return false
}

// prompt runs one turn in a chat's session. Stopping sends session/cancel and
// waits for the agent to confirm; one that doesn't is ended.
func (proc *agentProcess) prompt(ctx context.Context, cs *chatSession, req agent.TurnRequest, io agent.TurnIO) (agent.Result, error) {
	proc.mu.Lock()
	replay := cs.replay
	proc.mu.Unlock()
	blocks, err := proc.blocks(cs, req, io.TurnID, replay)
	if err != nil {
		return agent.Result{}, err
	}
	t := &turnState{io: io, file: cs.file, tools: map[string]*toolState{}}
	proc.mu.Lock()
	if cs.turn != nil {
		proc.mu.Unlock()
		return agent.Result{}, errors.New("the agent is still answering")
	}
	cs.turn, cs.replay = t, false
	proc.mu.Unlock()
	defer func() {
		proc.mu.Lock()
		cs.turn, proc.lastUsed = nil, time.Now()
		proc.mu.Unlock()
	}()

	type answer struct {
		result promptResult
		err    error
	}
	done := make(chan answer, 1)
	// The call outlives ctx: a stopped turn still waits for the agent's reply.
	call := context.WithoutCancel(ctx)
	go func() {
		var r promptResult
		err := proc.client.Call(call, methodPrompt, promptParams{SessionID: cs.id, Prompt: blocks}, &r)
		done <- answer{r, err}
	}()

	var got answer
	select {
	case got = <-done:
	case <-ctx.Done():
		_ = proc.client.Notify(call, methodCancel, cancelParams{SessionID: cs.id})
		timer := time.NewTimer(cancelGrace)
		defer timer.Stop()
		select {
		case got = <-done:
		case <-timer.C:
			proc.close()
		}
		t.finish()
		return t.result(stopCancelled), ctx.Err()
	}
	t.finish()
	if got.err != nil {
		return t.result(""), failure{name: proc.name, step: "couldn't answer", err: got.err}
	}
	return t.result(got.result.StopReason), nil
}

// blocks is the prompt as ACP content: the text, each image the agent can
// see, and a link to every other file.
func (proc *agentProcess) blocks(cs *chatSession, req agent.TurnRequest, turnID string, replay bool) ([]contentBlock, error) {
	text := req.Prompt
	if replay {
		events, _ := readEvents(cs.file.path)
		text = earlier(events, turnID) + text
	}
	var out []contentBlock
	if text != "" {
		out = append(out, contentBlock{Type: "text", Text: text})
	}
	for _, a := range req.Attachments {
		if a.IsImage() && proc.caps.PromptCapabilities.Image {
			raw, err := os.ReadFile(a.Path)
			if err != nil {
				return nil, err
			}
			out = append(out, contentBlock{Type: "image", MimeType: a.MimeType, Data: base64.StdEncoding.EncodeToString(raw)})
			continue
		}
		out = append(out, contentBlock{Type: "resource_link", Name: a.Name, URI: "file://" + a.Path, MimeType: a.MimeType})
	}
	return out, nil
}

// earlier is the conversation before turnID, for an agent that lost its own
// copy; the newest messages are kept when it is long.
func earlier(events []agent.Event, turnID string) string {
	var b strings.Builder
	for _, e := range events {
		if e.TurnID == turnID {
			continue
		}
		switch e.Kind {
		case agent.EventUserMessage:
			b.WriteString("User: " + e.Text + "\n\n")
		case agent.EventText:
			b.WriteString("You: " + e.Text + "\n\n")
		}
	}
	history := b.String()
	if history == "" {
		return ""
	}
	if len(history) > historyLimit {
		history = history[len(history)-historyLimit:]
	}
	return "Your session restarted. The conversation so far:\n\n" + history + "The user's new message:\n\n"
}

// running is the turn a session's update or request belongs to; nil while
// the session loads or runs nothing.
func (proc *agentProcess) running(sessionID string) *turnState {
	proc.mu.Lock()
	defer proc.mu.Unlock()
	cs := proc.bySession[sessionID]
	if cs == nil || cs.loading {
		return nil
	}
	return cs.turn
}

// notify reads the agent's session/update notifications into the turn of
// the session they name.
func (proc *agentProcess) notify(n stdiorpc.Notification) error {
	if n.Method != methodUpdate {
		return nil
	}
	var p updateParams
	if json.Unmarshal(n.Params, &p) != nil {
		return nil
	}
	if p.Update.SessionUpdate == updateConfig {
		proc.mu.Lock()
		cs := proc.bySession[p.SessionID]
		proc.mu.Unlock()
		if cs != nil {
			proc.setModels(cs, p.Update.ConfigOptions)
		}
		return nil
	}
	if t := proc.running(p.SessionID); t != nil {
		t.update(p.Update)
	}
	return nil
}

// request answers what the agent asks of the client: permission and
// questions. initialize declined file and terminal access.
func (proc *agentProcess) request(ctx context.Context, r stdiorpc.Request) (any, error) {
	if r.Method == methodElicit {
		var p elicitParams
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return nil, err
		}
		t := proc.running(p.SessionID)
		if t == nil {
			return elicitResult{Action: "cancel"}, nil
		}
		return t.ask(ctx, strings.Trim(string(r.ID), `"`), p), nil
	}
	if r.Method != methodPermission {
		return nil, &stdiorpc.Error{Code: -32601, Message: "RepoGo does not answer " + r.Method}
	}
	var p permissionParams
	if err := json.Unmarshal(r.Params, &p); err != nil {
		return nil, err
	}
	cancelled := permissionResult{Outcome: permissionOutcome{Outcome: "cancelled"}}
	t := proc.running(p.SessionID)
	if t == nil {
		return cancelled, nil
	}
	picked, ok := t.permission(ctx, p)
	if !ok {
		return cancelled, nil
	}
	return permissionResult{Outcome: permissionOutcome{Outcome: "selected", OptionID: picked}}, nil
}

// reapable is idle: no chat's turn running, none for 30 minutes.
func (proc *agentProcess) reapable(cutoff time.Time) bool {
	if proc.busy() {
		return false
	}
	proc.mu.Lock()
	defer proc.mu.Unlock()
	return proc.lastUsed.Before(cutoff)
}

func (proc *agentProcess) exited() bool {
	select {
	case <-proc.client.Done():
		return true
	default:
		return false
	}
}

func (proc *agentProcess) isClosed() bool { proc.mu.Lock(); defer proc.mu.Unlock(); return proc.closed }

func (proc *agentProcess) close() {
	proc.mu.Lock()
	proc.closed = true
	proc.mu.Unlock()
	_ = proc.client.Close()
}
