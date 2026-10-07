package claude

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/claudecode"
)

type runner struct {
	deps       agent.Dependencies
	pool       agent.SessionPool[*liveSession]
	executable func() string
	home       string
	// adopt is the Manager's, set once as it starts: see agent.Joiner.
	adopt func(chatID, cwd string)
}

func (r *runner) Kind() agent.Kind { return agent.KindClaude }
func (r *runner) Available() error {
	if r.deps.Context == nil || r.deps.Log == nil {
		return errors.New("Claude direct runner requires host context and logger")
	}
	if _, err := exec.LookPath(r.executable()); err != nil {
		return errors.New("Claude CLI not installed")
	}
	return nil
}
func (r *runner) Close() { r.pool.Close() }

func (r *runner) Send(ctx context.Context, req agent.TurnRequest, io agent.TurnIO) (agent.Result, error) {
	s, err := r.acquireSession(ctx, req)
	if err != nil {
		return agent.Result{}, err
	}
	defer r.pool.Release(s.key, s)
	result, err := s.send(ctx, req, io)
	if err != nil {
		r.pool.Retire(s.key, s)
	}
	return result, err
}

func (r *runner) OwnTurns(adopt func(chatID, cwd string)) { r.adopt = adopt }

// Join runs the turn Claude started by itself in chatID's process, between
// the host's turns, so it streams and stops as the host's own.
func (r *runner) Join(ctx context.Context, chatID string, io agent.TurnIO) (agent.Result, error) {
	s, ok := r.pool.Get(chatID)
	if !ok {
		return agent.Result{}, errors.New("Claude session closed")
	}
	defer r.pool.Release(chatID, s)
	return s.join(ctx, io)
}

func (r *runner) acquireSession(ctx context.Context, req agent.TurnRequest) (*liveSession, error) {
	epoch := r.pool.Epoch()
	var servers []agent.MCPServer
	if r.deps.MCP != nil {
		servers = r.deps.MCP(ctx, req.Cwd)
	}
	own := r.deps.Tools != nil && r.deps.Tools.On(req.Cwd)
	raw, _ := json.Marshal(struct {
		Servers []agent.MCPServer
		Own     bool
	}{servers, own})
	sum := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(sum[:])
	if s, ok := r.pool.Get(req.ChatID); ok {
		s.mu.Lock()
		matches := s.cwd == req.Cwd && (req.SessionID == "" || req.SessionID == s.id)
		same := matches && s.fingerprint == fingerprint && !s.closed
		busy := s.busyLocked()
		s.mu.Unlock()
		if same {
			select {
			case <-s.client.Done():
			default:
				return s, nil
			}
		}
		if busy {
			r.pool.Release(req.ChatID, s)
			return nil, errors.New("Claude session configuration changed while background work is active; stop or wait for it to finish")
		}
		if matches && req.SessionID == "" {
			req.SessionID = s.id
		}
		r.pool.Retire(req.ChatID, s)
	}
	s, err := r.startSession(ctx, req, servers, own)
	if err != nil {
		return nil, err
	}
	s.fingerprint = fingerprint
	if !r.pool.Put(epoch, s.key, s, s.reapable, s.close) {
		s.close()
		return nil, errors.New("Claude session closed during startup")
	}
	go func() {
		<-s.client.Done()
		exit := s.client.Exit()
		r.deps.Log.Info("Claude process exited", "chat", s.key, "code", exit.Code, "signal", exit.Signal, "expected", s.isClosed(), "error", exit.Err, "stderr", diagnosticStderr(exit.Stderr))
		r.pool.Retire(s.key, s)
	}()
	return s, nil
}

func (r *runner) startSession(ctx context.Context, req agent.TurnRequest, servers []agent.MCPServer, own bool) (*liveSession, error) {
	id := req.SessionID
	if id == "" {
		id = uuid.NewString()
	}
	s := newLiveSession(id, req.Cwd)
	s.key = agent.ChatID(agent.KindClaude, id)
	s.ownTurn = func() {
		r.deps.Log.Info("Claude started a turn by itself", "chat", s.key)
		r.adopt(s.key, s.cwd)
	}
	if own {
		entry, release := r.deps.Tools.Attach(s)
		s.release = release
		servers = append(slices.Clone(servers), entry)
	}
	configured := map[string]claudecode.MCPServer{}
	for _, server := range servers {
		headers := map[string]string{}
		for _, h := range server.Headers {
			headers[h.Name] = h.Value
		}
		configured[server.Name] = claudecode.MCPServer{Type: server.Type, URL: server.URL, Headers: headers}
	}
	env := r.environment()
	options := claudecode.Options{Executable: r.executable(), Cwd: req.Cwd, Env: env,
		SettingSources: []string{"user", "project", "local"}, AllowBypass: os.Geteuid() != 0 || os.Getenv("IS_SANDBOX") != "", MCPServers: configured}
	s.allowBypass = options.AllowBypass
	if req.SessionID == "" {
		options.SessionID = id
	} else {
		options.ResumeID = id
	}
	client, err := claudecode.Start(r.deps.Context, options, claudecode.Handlers{CanUseTool: s.respond, Elicitation: s.elicitation})
	if err != nil {
		if s.release != nil {
			s.release()
		}
		return nil, err
	}
	s.client = client
	go s.consume()
	startup, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	s.catalog, err = client.Initialize(startup)
	if err != nil {
		s.close()
		return nil, fmt.Errorf("initialize Claude: %w", err)
	}
	return s, nil
}

func (r *runner) environment() []string {
	env := append(agent.ChildEnv(), r.deps.Env...)
	// Claude reports running and idle only when asked: running is how the host
	// sees a turn Claude starts by itself, and idle settles a joined one.
	env = append(env, "CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS=1")
	// An explicit config directory changes account discovery even at the default path.
	if r.deps.Root != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+r.home)
	}
	return env
}

func (s *liveSession) applyConfig(ctx context.Context, cfg agent.TurnConfig) error {
	if cfg.Model != "" {
		if err := s.client.SetModel(ctx, cfg.Model); err != nil {
			if !rejectedConfig(err) {
				return err
			}
		} else {
			s.mu.Lock()
			s.lastModel = cfg.Model
			s.mu.Unlock()
		}
	}
	s.mu.Lock()
	selected := s.lastModel
	s.mu.Unlock()
	if selected == "" {
		selected = "default"
	}
	var model *claudecode.Model
	for i := range s.catalog.Models {
		m := &s.catalog.Models[i]
		if m.Value == selected || m.ResolvedModel == selected {
			model = m
			break
		}
	}
	settings := map[string]any{"fastMode": cfg.FastMode}
	if cfg.ReasoningLevel != "" && (model == nil || model.SupportsEffort && slices.Contains(model.Efforts, cfg.ReasoningLevel)) {
		settings["effortLevel"] = cfg.ReasoningLevel
	}
	if err := s.client.ApplyFlagSettings(ctx, settings); err != nil {
		if !rejectedConfig(err) {
			return err
		}
	}
	mode := map[agent.PermissionMode]string{agent.PermissionApprovalRequired: "default", agent.PermissionAutoAcceptEdits: "acceptEdits", agent.PermissionFullAccess: "bypassPermissions"}[cfg.PermissionMode]
	if cfg.Mode == "plan" {
		mode = "plan"
	}
	if mode != "" {
		if err := s.client.SetPermissionMode(ctx, mode); err != nil {
			if !rejectedConfig(err) {
				return err
			}
			return nil
		}
		s.mu.Lock()
		if mode == "plan" && s.mode != "plan" {
			s.prePlanMode = s.mode
		}
		s.mode = mode
		s.mu.Unlock()
	}
	return nil
}

func rejectedConfig(err error) bool {
	var rejected *claudecode.ControlError
	return errors.As(err, &rejected)
}

func processError(exit claudecode.Exit) error {
	if exit.Err != nil {
		return fmt.Errorf("Claude exited (code %d, signal %s): %w", exit.Code, exit.Signal, exit.Err)
	}
	return fmt.Errorf("Claude exited before completing the turn (code %d)", exit.Code)
}

func resultError(m claudecode.Message) error {
	if !m.IsError {
		return nil
	}
	detail := strings.Join(m.Errors, "; ")
	if detail == "" {
		detail = m.Result
	}
	if detail == "" {
		detail = m.Subtype
	}
	return fmt.Errorf("Claude: %s", detail)
}
