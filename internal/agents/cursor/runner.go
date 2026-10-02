package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/cursoragent"
)

func (p *Provider) Send(ctx context.Context, req agent.TurnRequest, io agent.TurnIO) (agent.Result, error) {
	if err := p.Available(); err != nil {
		return agent.Result{}, err
	}
	if req.Config.ReasoningLevel != "" || req.Config.ContextWindow != "" || req.Config.FastMode {
		return agent.Result{}, errors.New("Cursor model parameters must be selected through its model variant")
	}
	switch req.Config.PermissionMode {
	case "", agent.PermissionApprovalRequired, agent.PermissionAutoAcceptEdits, agent.PermissionFullAccess:
	default:
		return agent.Result{}, fmt.Errorf("unsupported Cursor permission mode %q", req.Config.PermissionMode)
	}
	s, err := p.acquire(ctx, req)
	if err != nil {
		return agent.Result{}, err
	}
	defer p.pool.Release(s.key, s)
	result, err := s.send(ctx, req, io)
	if err != nil {
		p.pool.Retire(s.key, s)
	}
	return result, err
}

func (p *Provider) acquire(ctx context.Context, req agent.TurnRequest) (*liveSession, error) {
	epoch := p.pool.Epoch()
	var servers []agent.MCPServer
	if p.deps.MCP != nil {
		servers = p.deps.MCP(ctx, req.Cwd)
	}
	own := p.deps.Tools != nil && p.deps.Tools.On(req.Cwd)
	raw, _ := json.Marshal(struct {
		Servers []agent.MCPServer
		Own     bool
	}{servers, own})
	fingerprint := sha256.Sum256(raw)
	if s, ok := p.pool.Get(req.ChatID); ok {
		s.mu.Lock()
		same := !s.closed && s.cwd == req.Cwd && (req.SessionID == "" || req.SessionID == s.id) && s.fingerprint == fingerprint
		s.mu.Unlock()
		if same {
			meta, err := p.sessions.meta(s.id)
			s.mu.Lock()
			same = err == nil && (s.io != nil || (s.nativeSize == meta.SizeBytes && s.nativeTime.Equal(meta.UpdatedAt)))
			s.mu.Unlock()
		}
		if same {
			select {
			case <-s.client.Done():
			default:
				return s, nil
			}
		}
		if req.SessionID == "" && s.cwd == req.Cwd {
			req.SessionID = s.id
		}
		p.pool.Retire(req.ChatID, s)
	}
	s := &liveSession{provider: p, cwd: req.Cwd, fingerprint: fingerprint, lastActivity: time.Now()}
	if own {
		entry, release := p.deps.Tools.Attach(s)
		s.release = release
		servers = append(servers, entry)
	}
	client, err := cursoragent.Start(p.deps.Context, cursoragent.Options{Executable: p.executable(), Cwd: req.Cwd, Env: p.environment()}, cursoragent.Handlers{Request: s.respond, Notification: s.handle})
	if err != nil {
		if s.release != nil {
			s.release()
		}
		return nil, err
	}
	s.client = client
	open, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	init, err := client.Initialize(open)
	if err != nil {
		s.close()
		return nil, fmt.Errorf("initialize Cursor: %w", err)
	}
	if req.SessionID != "" && !init.AgentCapabilities.LoadSession {
		s.close()
		return nil, errors.New("Cursor does not support session loading")
	}
	s.images = init.AgentCapabilities.PromptCapabilities.Image
	if req.SessionID != "" {
		meta, err := p.sessions.meta(req.SessionID)
		if err != nil {
			s.close()
			return nil, err
		}
		if meta.Cwd != req.Cwd {
			s.close()
			return nil, errors.New("Cursor session belongs to another project")
		}
	}
	s.mu.Lock()
	s.id, s.replaying = req.SessionID, true
	s.resetToolsLocked()
	s.mu.Unlock()

	state, err := client.Open(open, cursoragent.SessionParams{SessionID: req.SessionID, Cwd: req.Cwd, MCPServers: mcpServers(servers)})
	if err != nil {
		s.close()
		return nil, fmt.Errorf("open Cursor session: %w", err)
	}
	s.mu.Lock()
	s.id, s.key, s.state = state.SessionID, agent.ChatID(agent.KindCursor, state.SessionID), state
	s.finishReplayLocked()
	s.mu.Unlock()
	if !p.pool.Put(epoch, s.key, s, s.reapable, s.close) {
		s.close()
		return nil, errors.New("Cursor session closed during startup")
	}
	go func() {
		<-client.Done()
		exit := client.Exit()
		p.deps.Log.Info("Cursor process exited", "chat", s.key, "code", exit.Code, "signal", exit.Signal, "error", exit.Err)
		p.pool.Retire(s.key, s)
	}()
	return s, nil
}

func mcpServers(servers []agent.MCPServer) []cursoragent.MCPServer {
	out := make([]cursoragent.MCPServer, 0, len(servers))
	for _, v := range servers {
		s := cursoragent.MCPServer{Type: v.Type, Name: v.Name, URL: v.URL, Headers: []cursoragent.Header{}}
		for _, h := range v.Headers {
			s.Headers = append(s.Headers, cursoragent.Header{Name: h.Name, Value: h.Value})
		}
		out = append(out, s)
	}
	return out
}
