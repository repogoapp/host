// Package repogomcp is RepoGo's own MCP server, served by the host on the
// user's machine to the agents it runs. Its tools act for the turn that calls
// them, so each agent session gets an entry with its own bearer, and a call
// finds its turn through it.
package repogomcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/plugins"
)

// Path is where the host's loopback server mounts the handler.
const Path = "/mcp/repogo"

// JSON-RPC's own code for a body that is not JSON; the host's catalog has no use for it.
const codeParseError = -32700

// The MCP revisions this server speaks; a client asking for another gets the newest.
var protocols = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

const instructions = "RepoGo's own tools. `browser` drives the browser in the RepoGo app " +
	"on the phone that sent this turn."

type Config struct {
	// URL is the handler's address on the loopback server, known once it listens.
	URL func() string
	// On reports whether the server is on for a folder; the host switches it for the whole environment.
	On      func(cwd string) bool
	Browser *Browser
	// Plugins runs project plugins; a host without the SDK offers no plugin tools.
	Plugins *plugins.Service
	Log     *slog.Logger
}

type Server struct {
	cfg Config

	mu       sync.Mutex
	sessions map[string]attached
}

// attached is one session and the folder it was opened in, which decides its
// tool list before any turn runs.
type attached struct {
	session agent.ToolSession
	cwd     string
}

func New(cfg Config) *Server {
	return &Server{cfg: cfg, sessions: map[string]attached{}}
}

func (s *Server) On(cwd string) bool { return s.cfg.On(cwd) }

// Attach mints the bearer that names one session. It never leaves this
// machine: the agent process gets it as a header, and it dies with the session.
func (s *Server) Attach(session agent.ToolSession, cwd string) (agent.MCPServer, func()) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	s.mu.Lock()
	s.sessions[token] = attached{session: session, cwd: cwd}
	s.mu.Unlock()
	entry := agent.MCPServer{
		Type: "http", Name: agent.ToolsServer, URL: s.cfg.URL(),
		Headers: []agent.Header{{Name: "Authorization", Value: "Bearer " + token}},
	}
	return entry, func() {
		s.mu.Lock()
		delete(s.sessions, token)
		s.mu.Unlock()
	}
}

func (s *Server) session(r *http.Request) (attached, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return attached{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	session, ok := s.sessions[token]
	return session, ok
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// ServeHTTP is Streamable HTTP without a stream: every request is answered
// with one JSON body, which the transport allows and every client accepts.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	session, ok := s.session(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, jsonrpc.MaxMessageBytes))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, nil, nil, &jsonrpc.Error{Code: codeParseError, Message: "parse error"})
		return
	}
	// A notification or a response to us: nothing to answer.
	if len(req.ID) == 0 || req.Method == "" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rpcErr := s.handle(r.Context(), session, req)
	writeRPC(w, req.ID, result, rpcErr)
}

// withPlugins reports whether a session gets the plugin tools: on a host that
// runs plugins, in every project, so an agent can build a project's first one.
func (s *Server) withPlugins() bool {
	return s.cfg.Plugins.On()
}

func (s *Server) handle(ctx context.Context, session attached, req rpcRequest) (any, *jsonrpc.Error) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := protocols[0]
		if slices.Contains(protocols, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": agent.ToolsServer, "version": "2"},
			"instructions":    s.instructions(session.cwd),
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		tools := []tool{browserTool}
		if s.withPlugins() {
			tools = append(tools, pluginsTool, pluginCallTool)
		}
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: "invalid params"}
		}
		return s.call(ctx, session.session, p.Name, p.Arguments), nil
	}
	return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method not found: " + req.Method}
}

func (s *Server) instructions(cwd string) string {
	if s.withPlugins() {
		return instructions + pluginsInstructions
	}
	return instructions
}

// call runs a tool for the session's current turn. Failures are results, not
// protocol errors, so the agent reads them and can react.
func (s *Server) call(ctx context.Context, session agent.ToolSession, name string, args json.RawMessage) toolResult {
	turn, ok := session.Running()
	if !ok {
		return failed("RepoGo: no turn is running in this chat")
	}
	// A session keeps its entry until its next turn, so switching the server
	// off must also stop calls from the turn already running.
	if !s.cfg.On(turn.Cwd) {
		return failed("RepoGo's tools are switched off on this environment")
	}
	// Bounded by both the turn and the agent's request: a stopped turn or a
	// dropped call stops waiting on the phone.
	callCtx, cancel := context.WithCancel(turn.Ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	turn.Ctx = callCtx

	switch name {
	case browserTool.Name:
		return s.cfg.Browser.Tool(turn, args)
	case pluginsTool.Name, pluginCallTool.Name:
		if s.withPlugins() {
			return pluginsTools(s.cfg.Plugins, turn, name, args)
		}
	}
	return failed("unknown tool: " + name)
}

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// toolResult is MCP's CallToolResult.
type toolResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

type content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

func failed(message string) toolResult {
	return toolResult{Content: []content{{Type: "text", Text: message}}, IsError: true}
}

// succeeded is the payload as JSON: lists carry ids the model passes back,
// and prose invites it to mangle them.
func succeeded(payload any) toolResult {
	b, _ := json.MarshalIndent(payload, "", "  ")
	return toolResult{Content: []content{{Type: "text", Text: string(b)}}}
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *jsonrpc.Error) {
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if id == nil {
		out["id"] = nil
	}
	if rpcErr != nil {
		out["error"] = rpcErr
	} else {
		out["result"] = result
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
