// Package acp runs agents that speak the Agent Client Protocol over stdio:
// the environment's own, named by an agent.json in a plugin folder of
// ~/.repogo/plugins (a repo's own agent.json never runs), and ones the host
// ships, such as Antigravity, through a Config of their own. The host keeps
// each chat's transcript itself, so history never depends on what the agent
// remembers.
package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/apphome"
	"github.com/repogoapp/host/internal/projectagent"
	"github.com/repogoapp/host/internal/session"
	"github.com/repogoapp/host/internal/toollabel"
)

// Config is one kind of agent this runner serves: what differs between the
// environment's own agents and one the host ships.
type Config struct {
	Kind agent.Kind
	Name string
	// Home is its folder in the host's state: chat transcripts and the
	// models its agents reported.
	Home string
	// Find is the agent a chat in cwd runs; id names one of the environment's
	// own agents, and is empty for a shipped agent.
	Find func(cwd, id string) (projectagent.Manifest, error)

	// Settings is what a turn sets before its prompt, by session option
	// category (mode, thought_level): RepoGo's config in the agent's words,
	// most wanted first. The first value the option offers is set; none
	// leaves it alone.
	Settings func(agent.TurnConfig) map[string][]string
	// Allows reports a tool call a turn runs without asking, by the agent's
	// name for the tool.
	Allows func(cfg agent.TurnConfig, tool string) bool
	// OwnTools hands sessions RepoGo's own MCP server and RepoGo's
	// instructions, as Claude and Codex get them; SessionMeta is how the
	// agent takes the instructions, as session/new's _meta.
	OwnTools    bool
	SessionMeta func(instructions string) any
	// Steers is set for an agent that takes a prompt sent during a turn into
	// that turn.
	Steers bool
	// MCPOverHTTP is set for an agent that takes HTTP MCP servers though its
	// initialize doesn't say so.
	MCPOverHTTP bool
	// Label names a tool call by the agent's own name for the tool and its
	// input: the row's icon and lines, and the input as the shared labels read
	// it (path, command, url). Set, calls are recorded under the tool's name
	// and labelled by it; unset, by the title the agent gave each call.
	Label func(tool string, input json.RawMessage) (toollabel.Label, map[string]any, bool)
	// Adopt finds a chat the agent started outside RepoGo (in a terminal), by
	// the agent's own session id: the folder it ran in. A turn sent to one
	// makes it RepoGo's, reopening the agent's session.
	Adopt func(session string) (cwd string, ok bool)
}

// Provider is this host's ACP runner and the transcripts it writes.
type Provider struct {
	deps agent.Dependencies
	cfg  Config
	// pool is each agent's process by its manifest's Key; starting holds one
	// start per Key at a time, since a second instance would refuse to run.
	pool     agent.SessionPool[*agentProcess]
	starting sync.Map
	// live is the process each running turn is on, by chat key, for a steer.
	live     sync.Map
	sessions *Sessions
	reported *reported
	// plugins is the environment's plugins folder, whose agents every
	// project is offered; a repo's own are never read.
	plugins string
}

// home is the environment agents' folder in the host's state.
const home = "acp"

// New builds this host's runner for the environment's own agents. A Root in
// deps (tests, the hermetic test host) keeps its files under it.
func New(deps agent.Dependencies) *Provider {
	plugins := filepath.Join(deps.Root, projectagent.PluginsFolder)
	if deps.Root == "" {
		plugins, _ = apphome.Path(projectagent.PluginsFolder)
	}
	p := NewWith(deps, Config{Kind: agent.KindProject, Name: "Project agent", Home: home,
		Find: func(cwd, id string) (projectagent.Manifest, error) {
			if id == "" {
				return projectagent.Manifest{}, errors.New("choose one of the project's agents")
			}
			return projectagent.Find(cwd, plugins, id)
		}})
	p.plugins = plugins
	return p
}

// NewWith builds a runner for one kind of agent.
func NewWith(deps agent.Dependencies, cfg Config) *Provider {
	dir := filepath.Join(deps.Root, cfg.Home)
	if deps.Root == "" {
		dir, _ = apphome.Path(cfg.Home)
	}
	return &Provider{
		deps:     deps,
		cfg:      cfg,
		sessions: newSessions(filepath.Join(dir, "chats"), cfg.Kind),
		reported: &reported{path: filepath.Join(dir, "models.json")},
	}
}

func (p *Provider) Kind() agent.Kind { return p.cfg.Kind }
func (p *Provider) Name() string     { return p.cfg.Name }

// Available is true wherever the host runs: each project brings its own
// agents, and one that can't start says why in its turn.
func (p *Provider) Available() error {
	if p.deps.Context == nil || p.deps.Log == nil {
		return errors.New("ACP runner requires host context and logger")
	}
	return nil
}

// Sessions reads the transcripts of this host's ACP chats.
func (p *Provider) Sessions() session.Provider { return p.sessions }

// Chats is the same transcripts, for a shipped agent's own history reader.
func (p *Provider) Chats() *Sessions { return p.sessions }

func (p *Provider) Close() { p.pool.Close() }

// chat is the one a turn runs in: its id, agent and transcript.
type chat struct {
	id       string
	manifest projectagent.Manifest
	file     *transcript
	// session is the agent's own id from an earlier process, if any.
	session string
	kind    agent.Kind
}

func (c chat) key() string { return agent.ChatID(c.kind, c.id) }

// Send runs one turn on the chat's agent process, starting one if needed.
func (p *Provider) Send(ctx context.Context, req agent.TurnRequest, io agent.TurnIO) (agent.Result, error) {
	c, err := p.open(req, io)
	if err != nil {
		return agent.Result{}, err
	}
	if err := c.file.event(agent.Event{Kind: agent.EventTurnStarted, TurnID: io.TurnID}); err != nil {
		return agent.Result{}, err
	}
	if err := c.file.event(agent.Event{Kind: agent.EventUserMessage, TurnID: io.TurnID, Text: req.Prompt}); err != nil {
		return agent.Result{}, err
	}
	result, err := p.run(ctx, c, req, io)
	result.SessionID = c.id
	end := agent.Event{Kind: agent.EventTurnFinished, TurnID: io.TurnID, Usage: result.Usage}
	if err != nil {
		end = agent.Event{Kind: agent.EventTurnFailed, TurnID: io.TurnID, Error: err.Error()}
	}
	if werr := c.file.event(end); werr != nil && err == nil {
		err = werr
	}
	return result, err
}

func (p *Provider) run(ctx context.Context, c chat, req agent.TurnRequest, io agent.TurnIO) (agent.Result, error) {
	proc, err := p.acquire(ctx, c.manifest, c.manifest.DefaultModel(req.Config.Model), io)
	if err != nil {
		return agent.Result{}, err
	}
	defer p.pool.Release(proc.key, proc)
	p.live.Store(c.key(), proc)
	defer p.live.Delete(c.key())
	result, err := p.turn(ctx, proc, c, req, io)
	if err != nil {
		var f failure
		if errors.As(err, &f) {
			p.deps.Log.Info("ACP agent failed", "agent", c.manifest.ID, "step", f.step, "error", detail(err, proc.client))
		}
		if proc.exited() {
			p.pool.Retire(proc.key, proc)
		}
	}
	return result, err
}

// turn opens the chat's session in the process, on the model asked for, and
// runs the prompt.
func (p *Provider) turn(ctx context.Context, proc *agentProcess, c chat, req agent.TurnRequest, io agent.TurnIO) (agent.Result, error) {
	cs, err := proc.session(ctx, c, p.servers(ctx, c, proc, req), p.meta(req))
	if err != nil {
		return agent.Result{}, err
	}
	if err := proc.useModel(ctx, cs, req.Config.Model); err != nil {
		return agent.Result{}, err
	}
	if p.cfg.Settings != nil {
		if err := proc.useSettings(ctx, cs, p.cfg.Settings(req.Config)); err != nil {
			return agent.Result{}, err
		}
	}
	if id, name := proc.modelInUse(cs, c.manifest); id != "" {
		_ = c.file.append(line{Type: lineModel, Model: id, ModelName: name})
		if io.Session != nil {
			io.Session(c.id, id)
		}
	}
	return proc.prompt(ctx, cs, req, io, func(tool string) bool {
		return p.cfg.Allows != nil && p.cfg.Allows(req.Config, tool)
	}, p.cfg.Label)
}

// Steer sends req into its chat's running turn, for an agent that takes one.
func (p *Provider) Steer(_ context.Context, turnID string, req agent.TurnRequest) error {
	if !p.cfg.Steers {
		return fmt.Errorf("%s takes no message during a turn", p.cfg.Name)
	}
	v, ok := p.live.Load(req.ChatID)
	if !ok {
		return errors.New("no turn to steer")
	}
	return v.(*agentProcess).steer(req.ChatID, turnID, req)
}

// open finds the chat a turn belongs to. A new one gets its id and file here,
// before anything runs, so the phone can follow the turn and its questions.
func (p *Provider) open(req agent.TurnRequest, io agent.TurnIO) (chat, error) {
	if req.SessionID != "" {
		path := p.sessions.path(req.SessionID)
		info, err := readInfo(path)
		if errors.Is(err, os.ErrNotExist) && p.cfg.Adopt != nil {
			if cwd, ok := p.cfg.Adopt(req.SessionID); ok {
				info, err = p.adopt(req.SessionID, cwd)
			}
		}
		if err != nil {
			return chat{}, fmt.Errorf("open chat: %w", err)
		}
		m, err := p.cfg.Find(info.Cwd, p.agentID(info.Agent))
		if err != nil {
			return chat{}, err
		}
		// Every turn names its chat: the phone's live view of a turn starts here.
		if io.Session != nil {
			io.Session(req.SessionID, "")
		}
		return chat{id: req.SessionID, manifest: m, file: &transcript{path: path}, session: info.Session, kind: p.cfg.Kind}, nil
	}
	m, err := p.cfg.Find(req.Cwd, req.ProjectAgent)
	if err != nil {
		return chat{}, err
	}
	id := uuid.NewString()
	file, err := createTranscript(p.sessions.path(id), m.ID, req.Cwd)
	if err != nil {
		return chat{}, err
	}
	if io.Session != nil {
		io.Session(id, "")
	}
	return chat{id: id, manifest: m, file: file, kind: p.cfg.Kind}, nil
}

// adopt starts the host's file for a chat the agent began on its own, under
// the agent's id for it, so its next turn reopens that session.
func (p *Provider) adopt(session, cwd string) (chatInfo, error) {
	if _, err := p.sessions.Adopt(session, cwd); err != nil {
		return chatInfo{}, err
	}
	return readInfo(p.sessions.path(session))
}

// agentID is the agent a chat's file names, as Find takes it: a shipped
// agent's Find takes none.
func (p *Provider) agentID(recorded string) string {
	if p.cfg.Kind != agent.KindProject {
		return ""
	}
	return recorded
}

// acquire returns the agent's running process, or starts one: it outlives
// the turn, so each chat's session keeps its context between messages. A
// manifest model is the whole process's, so another one restarts it.
func (p *Provider) acquire(ctx context.Context, m projectagent.Manifest, model string, io agent.TurnIO) (*agentProcess, error) {
	key := m.Key()
	lock, _ := p.starting.LoadOrStore(key, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	epoch := p.pool.Epoch()
	if proc, ok := p.pool.Get(key); ok {
		if !proc.exited() && proc.fingerprint == m.Fingerprint && proc.model == model {
			return proc, nil
		}
		if !proc.exited() && proc.model != model && proc.busy() {
			p.pool.Release(key, proc)
			return nil, fmt.Errorf("%s is answering another chat with %s; send again when it's done", m.Name, proc.model)
		}
		p.pool.Release(key, proc)
		p.pool.Retire(key, proc)
	}
	proc, err := p.start(ctx, m, model)
	if err != nil {
		return nil, err
	}
	if !p.pool.Put(epoch, proc.key, proc, proc.reapable, proc.close) {
		proc.close()
		return nil, fmt.Errorf("%s closed during startup", m.Name)
	}
	go func() {
		<-proc.client.Done()
		exit := proc.client.Exit()
		p.deps.Log.Info("ACP agent exited", "agent", m.ID, "dir", m.Dir, "code", exit.Code,
			"signal", exit.Signal, "expected", proc.isClosed(), "error", exit.Err, "stderr", lastLine(exit.Stderr))
		p.pool.Retire(proc.key, proc)
	}()
	return proc, nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// ProjectAgents is the project's agents for the picker, each with the models
// its manifest lists, else the ones it reported when it last ran.
func (p *Provider) ProjectAgents(path string) ([]projectagent.ProjectAgent, error) {
	agents, err := projectagent.List(path, p.plugins)
	if err != nil {
		return nil, err
	}
	for i, a := range agents {
		if a.Error != "" || len(a.Models) > 0 {
			continue
		}
		m, err := projectagent.Find(path, p.plugins, a.ID)
		if err != nil {
			continue
		}
		if models, ok := p.reported.get(m.Fingerprint); ok {
			agents[i].Models, agents[i].Model = models.Models, models.Current
		}
	}
	return agents, nil
}

// ProjectAgentIcon is the icon in the agent's agent.json; "" when it has none
// or can't be read.
func (p *Provider) ProjectAgentIcon(path, name string) string {
	m, err := projectagent.Find(path, p.plugins, name)
	if err != nil {
		return ""
	}
	return m.Icon
}
