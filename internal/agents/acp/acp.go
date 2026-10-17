// Package acp runs a project's own agents: any program that speaks the Agent
// Client Protocol over stdio, named by `.repogo/agents/<name>/agent.json`.
// The host keeps each chat's transcript itself, so history never depends on
// what the agent remembers.
package acp

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/apphome"
	"github.com/repogoapp/host/internal/projectagent"
	"github.com/repogoapp/host/internal/session"
)

// Provider is this host's ACP runner and the transcripts it writes.
type Provider struct {
	deps agent.Dependencies
	// pool is each agent's process by its folder; starting holds one start
	// per folder at a time, since a second instance would refuse to run.
	pool     agent.SessionPool[*agentProcess]
	starting sync.Map
	sessions *Sessions
	reported *reported
}

// New builds this host's provider. A Root in deps (tests, the hermetic test
// host) keeps its transcripts and reported models under it.
func New(deps agent.Dependencies) *Provider {
	dir := filepath.Join(deps.Root, string(agent.KindACP))
	if deps.Root == "" {
		dir, _ = apphome.Path(string(agent.KindACP))
	}
	return &Provider{
		deps:     deps,
		sessions: newSessions(filepath.Join(dir, "chats")),
		reported: &reported{path: filepath.Join(dir, "models.json")},
	}
}

func (p *Provider) Kind() agent.Kind { return agent.KindACP }
func (p *Provider) Name() string     { return "Project agent" }

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

func (p *Provider) Close() { p.pool.Close() }

// chat is the one a turn runs in: its id, agent and transcript.
type chat struct {
	id       string
	manifest projectagent.Manifest
	file     *transcript
	// session is the agent's own id from an earlier process, if any.
	session string
}

func (c chat) key() string { return agent.ChatID(agent.KindACP, c.id) }

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
	cs, err := proc.session(ctx, c, p.mcpServers(ctx, c, proc.caps))
	if err != nil {
		return agent.Result{}, err
	}
	if err := proc.useModel(ctx, cs, req.Config.Model); err != nil {
		return agent.Result{}, err
	}
	result, err := proc.prompt(ctx, cs, req, io)
	if err != nil && proc.exited() {
		p.pool.Retire(proc.key, proc)
	}
	return result, err
}

// open finds the chat a turn belongs to. A new one gets its id and file here,
// before anything runs, so the phone can follow the turn and its questions.
func (p *Provider) open(req agent.TurnRequest, io agent.TurnIO) (chat, error) {
	if req.SessionID != "" {
		path := p.sessions.path(req.SessionID)
		info, err := readInfo(path)
		if err != nil {
			return chat{}, fmt.Errorf("open chat: %w", err)
		}
		m, err := projectagent.Find(info.Cwd, info.Agent)
		if err != nil {
			return chat{}, err
		}
		// Every turn names its chat: the phone's live view of a turn starts here.
		if io.Session != nil {
			io.Session(req.SessionID)
		}
		return chat{id: req.SessionID, manifest: m, file: &transcript{path: path}, session: info.Session}, nil
	}
	if req.ProjectAgent == "" {
		return chat{}, errors.New("choose one of the project's agents")
	}
	m, err := projectagent.Find(req.Cwd, req.ProjectAgent)
	if err != nil {
		return chat{}, err
	}
	id := uuid.NewString()
	file, err := createTranscript(p.sessions.path(id), m.ID, req.Cwd)
	if err != nil {
		return chat{}, err
	}
	if io.Session != nil {
		io.Session(id)
	}
	return chat{id: id, manifest: m, file: file}, nil
}

// acquire returns the agent's running process, or starts one: it outlives
// the turn, so each chat's session keeps its context between messages. A
// manifest model is the whole process's, so another one restarts it.
func (p *Provider) acquire(ctx context.Context, m projectagent.Manifest, model string, io agent.TurnIO) (*agentProcess, error) {
	lock, _ := p.starting.LoadOrStore(m.Dir, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	epoch := p.pool.Epoch()
	if proc, ok := p.pool.Get(m.Dir); ok {
		if !proc.exited() && proc.fingerprint == m.Fingerprint && proc.model == model {
			return proc, nil
		}
		if !proc.exited() && proc.model != model && proc.busy() {
			p.pool.Release(m.Dir, proc)
			return nil, fmt.Errorf("%s is answering another chat with %s; send again when it's done", m.Name, proc.model)
		}
		p.pool.Release(m.Dir, proc)
		p.pool.Retire(m.Dir, proc)
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
	agents, err := projectagent.List(path)
	if err != nil {
		return nil, err
	}
	for i, a := range agents {
		if a.Error != "" || len(a.Models) > 0 {
			continue
		}
		m, err := projectagent.Find(path, a.ID)
		if err != nil {
			continue
		}
		if models, ok := p.reported.get(m.Fingerprint); ok {
			agents[i].Models, agents[i].Model = models.Models, models.Current
		}
	}
	return agents, nil
}
