package codex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/codexappserver"
)

// A resumed thread replays its rollout before answering, so opening one may
// take longer than a fresh start.
const openTimeout = 60 * time.Second

// errOpenElsewhere is a resume refused because another Codex app is writing
// the thread; Codex lets one process write a thread at a time.
var errOpenElsewhere = errors.New("This chat is open in Codex on your environment. Close it there, then send again.")

type runner struct {
	deps       agent.Dependencies
	pool       agent.SessionPool[*liveSession]
	executable func() string
	home       string
}

func (r *runner) Kind() agent.Kind { return agent.KindCodex }
func (r *runner) Available() error {
	if r.deps.Context == nil || r.deps.Log == nil {
		return errors.New("Codex runner requires host context and logger")
	}
	if _, err := exec.LookPath(r.executable()); err != nil {
		return errors.New("Codex CLI not installed")
	}
	return nil
}
func (r *runner) Close() { r.pool.Close() }

// Send runs one turn on the chat's live app server, starting one if needed.
// The process outlives the turn, so the thread stays loaded and the
// provider's prompt cache survives.
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

// Steer adds req to the chat's running turn, which reads it before its next
// model request. An error means Codex did not take it.
func (r *runner) Steer(ctx context.Context, turnID string, req agent.TurnRequest) error {
	s, ok := r.pool.Get(req.ChatID)
	if !ok {
		return errors.New("no Codex session for the chat")
	}
	defer r.pool.Release(req.ChatID, s)
	return s.steer(ctx, turnID, req)
}

// acquireSession returns the chat's live session, or opens one when there is
// none, it is for another working tree or thread, or its MCP servers changed:
// a thread keeps the servers it opened with.
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
			return nil, errors.New("Codex session configuration changed while it is still answering; stop or wait for it to finish")
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
		return nil, errors.New("Codex session closed during startup")
	}
	go func() {
		<-s.client.Done()
		exit := s.client.Exit()
		r.deps.Log.Info("Codex process exited", "chat", s.key, "code", exit.Code, "signal", exit.Signal, "expected", s.isClosed(), "error", exit.Err, "stderr", lastLine(exit.Stderr))
		r.pool.Retire(s.key, s)
	}()
	return s, nil
}

// startSession starts an app server and opens the thread the chat names, or a
// new one. A resume that fails is the turn's error: a fresh thread would
// answer in a different conversation than the one on the user's screen.
func (r *runner) startSession(ctx context.Context, req agent.TurnRequest, servers []agent.MCPServer, own bool) (*liveSession, error) {
	if req.SessionID != "" && threadOpenElsewhere(r.home, req.SessionID) {
		return nil, errOpenElsewhere
	}
	s := newLiveSession(req.Cwd, r.deps.Log)
	if own {
		entry, release := r.deps.Tools.Attach(s)
		s.release = release
		servers = append(slices.Clone(servers), entry)
	}
	client, err := codexappserver.Start(r.deps.Context, codexappserver.Options{
		Executable: r.executable(), Cwd: req.Cwd, Env: r.environment(),
	}, codexappserver.Handlers{Request: s.respond})
	if err != nil {
		if s.release != nil {
			s.release()
		}
		return nil, err
	}
	s.client = client
	go s.consume()
	open, cancel := context.WithTimeout(ctx, openTimeout)
	defer cancel()
	if err = client.Initialize(open); err != nil {
		s.close()
		return nil, fmt.Errorf("initialize Codex: %w", withStderr(err, client))
	}
	params := codexappserver.ThreadParams{Cwd: req.Cwd, Config: mcpConfig(servers)}
	var thread codexappserver.ThreadResponse
	if req.SessionID == "" {
		thread, err = client.ThreadStart(open, params)
	} else {
		params.ThreadID = req.SessionID
		thread, err = client.ThreadResume(open, params)
	}
	if err != nil {
		s.close()
		if req.SessionID != "" && strings.Contains(err.Error(), "already has an active writer") {
			return nil, errOpenElsewhere
		}
		if req.SessionID != "" {
			return nil, fmt.Errorf("reopen Codex chat: %w", withStderr(err, client))
		}
		return nil, fmt.Errorf("start Codex chat: %w", withStderr(err, client))
	}
	if req.SessionID != "" && thread.Thread.ID != req.SessionID {
		s.close()
		return nil, fmt.Errorf("reopen Codex chat: Codex opened %s instead of %s", thread.Thread.ID, req.SessionID)
	}
	s.id = thread.Thread.ID
	s.key = agent.ChatID(agent.KindCodex, s.id)
	s.model = thread.Model
	if thread.ReasoningEffort != nil {
		s.effort = *thread.ReasoningEffort
	}
	return s, nil
}

// environment is the host's, plus what hooks read to know a device sent the
// turn. A test Root isolates Codex's home; otherwise the user's own is found.
func (r *runner) environment() []string {
	env := append(agent.ChildEnv(), r.deps.Env...)
	if r.deps.Root != "" {
		env = append(env, "CODEX_HOME="+r.home)
	}
	return env
}

// mcpConfig is the servers as config.toml's mcp_servers table, layered over
// the user's own servers rather than replacing them.
func mcpConfig(servers []agent.MCPServer) map[string]any {
	if len(servers) == 0 {
		return nil
	}
	table := map[string]any{}
	for _, server := range servers {
		headers := map[string]string{}
		for _, h := range server.Headers {
			headers[h.Name] = h.Value
		}
		table[server.Name] = map[string]any{"url": server.URL, "http_headers": headers}
	}
	return map[string]any{"mcp_servers": table}
}

func withStderr(err error, c *codexappserver.Client) error {
	if line := lastLine(c.Stderr()); line != "" {
		return fmt.Errorf("%w (%s)", err, line)
	}
	return err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
