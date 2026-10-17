package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The host as a client of a connected server, over Streamable HTTP: one
// session per server, opened on first use and opened again when the server
// forgets it.

const protocolVersion = "2025-06-18"

// maxReply bounds what is read from a server; a tool's answer goes to a phone.
const maxReply = 4 << 20

// toolsTTL is how long a server's tool list is trusted: its annotations
// decide what a card may run without asking.
const toolsTTL = 5 * time.Minute

// errExpired is a server answering 404 to a session it no longer has.
var errExpired = errors.New("mcp: session expired")

// rpcError is a server's JSON-RPC error.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// tool is what a card's call needs from a server's tool list.
type tool struct {
	Name        string `json:"name"`
	Title       string `json:"title"`
	Annotations struct {
		Title           string `json:"title"`
		ReadOnlyHint    *bool  `json:"readOnlyHint"`
		DestructiveHint *bool  `json:"destructiveHint"`
	} `json:"annotations"`
}

func (t tool) readOnly() bool {
	return t.Annotations.ReadOnlyHint != nil && *t.Annotations.ReadOnlyHint
}

// destructive is MCP's default for a tool that writes: true unless it says not.
func (t tool) destructive() bool {
	return !t.readOnly() && (t.Annotations.DestructiveHint == nil || *t.Annotations.DestructiveHint)
}

func (t tool) label() string {
	if t.Title != "" {
		return t.Title
	}
	if t.Annotations.Title != "" {
		return t.Annotations.Title
	}
	return t.Name
}

// session is one server's: its id once initialized, and its tools.
type session struct {
	// open makes the handshake one caller's job.
	open sync.Mutex

	mu      sync.Mutex
	id      string
	ready   bool
	tools   map[string]tool
	toolsAt time.Time
}

func (s *Service) sessionOf(id string) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[id] == nil {
		s.sessions[id] = &session{}
	}
	return s.sessions[id]
}

// request sends one JSON-RPC request in the server's session, opening it
// first, and once more in a new one when the server has forgotten it.
func (s *Service) request(ctx context.Context, n named, method string, params any) (json.RawMessage, error) {
	sess := s.sessionOf(n.record.ID)
	for attempt := 0; ; attempt++ {
		id, err := s.handshake(ctx, n, sess)
		if err != nil {
			return nil, err
		}
		result, _, err := s.post(ctx, n, id, method, params)
		if errors.Is(err, errExpired) && attempt == 0 {
			sess.mu.Lock()
			if sess.id == id {
				sess.ready, sess.id = false, ""
			}
			sess.mu.Unlock()
			continue
		}
		return result, err
	}
}

// handshake is the session's id, initializing it when it has none.
func (s *Service) handshake(ctx context.Context, n named, sess *session) (string, error) {
	sess.open.Lock()
	defer sess.open.Unlock()
	sess.mu.Lock()
	ready, id := sess.ready, sess.id
	sess.mu.Unlock()
	if ready {
		return id, nil
	}
	_, id, err := s.post(ctx, n, "", "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "repogo", "version": "2"},
	})
	if err != nil {
		return "", err
	}
	if _, _, err := s.post(ctx, n, id, "notifications/initialized", nil); err != nil {
		return "", err
	}
	sess.mu.Lock()
	sess.ready, sess.id = true, id
	sess.mu.Unlock()
	return id, nil
}

// post sends one message and reads its answer, from a JSON body or an event
// stream. A method starting "notifications/" is a notification: no answer.
// It returns the session id the server set, if any.
func (s *Service) post(ctx context.Context, n named, sessionID, method string, params any) (json.RawMessage, string, error) {
	message := map[string]any{"jsonrpc": "2.0", "method": method}
	notify := strings.HasPrefix(method, "notifications/")
	if !notify {
		message["id"] = 1
	}
	if params != nil {
		message["params"] = params
	}
	body, err := json.Marshal(message)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.record.URL, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
		req.Header.Set("MCP-Protocol-Version", protocolVersion)
	}
	for _, h := range n.headers {
		req.Header.Set(h.Name, h.Value)
	}
	res, err := s.calls.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", n.record.Label, err)
	}
	defer res.Body.Close()
	newID := res.Header.Get("Mcp-Session-Id")
	switch {
	case res.StatusCode == http.StatusNotFound && sessionID != "":
		return nil, "", errExpired
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return nil, "", ErrInvalid.Errorf("%s refused the sign-in; reconnect it in RepoGo's MCP settings", n.record.Label)
	case res.StatusCode >= 300:
		return nil, "", fmt.Errorf("mcp: %s answered %s", n.record.Label, res.Status)
	case notify:
		return nil, newID, nil
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcError       `json:"error"`
	}
	reply := io.LimitReader(res.Body, maxReply)
	if strings.Contains(res.Header.Get("Content-Type"), "text/event-stream") {
		err = readEvent(reply, &envelope)
	} else {
		err = json.NewDecoder(reply).Decode(&envelope)
	}
	if err != nil {
		return nil, "", fmt.Errorf("mcp: unreadable answer from %s: %w", n.record.Label, err)
	}
	if envelope.Error != nil {
		return nil, newID, envelope.Error
	}
	return envelope.Result, newID, nil
}

// readEvent decodes the stream's first answer, skipping the requests and
// notifications a server may send before it.
func readEvent(r io.Reader, into any) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxReply)
	var data strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if rest, ok := strings.CutPrefix(line, "data:"); ok {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(rest, " "))
			continue
		}
		if line != "" || data.Len() == 0 {
			continue
		}
		event := data.String()
		data.Reset()
		if isAnswer(event) {
			return json.Unmarshal([]byte(event), into)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if isAnswer(data.String()) {
		return json.Unmarshal([]byte(data.String()), into)
	}
	return io.ErrUnexpectedEOF
}

// isAnswer reports whether an event is a reply: an id and no method.
func isAnswer(event string) bool {
	var probe struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	return json.Unmarshal([]byte(event), &probe) == nil && probe.Method == "" && len(probe.ID) > 0
}

// toolsOf is a server's tools by name, fetched again when stale; fresh asks
// for a new list, for a name the cached one lacks.
func (s *Service) toolsOf(ctx context.Context, n named, fresh bool) (map[string]tool, error) {
	sess := s.sessionOf(n.record.ID)
	sess.mu.Lock()
	tools, at := sess.tools, sess.toolsAt
	sess.mu.Unlock()
	if !fresh && tools != nil && time.Since(at) < toolsTTL {
		return tools, nil
	}
	tools = map[string]tool{}
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := s.request(ctx, n, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools      []tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("mcp: unreadable tool list from %s: %w", n.record.Label, err)
		}
		for _, t := range page.Tools {
			tools[t.Name] = t
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	sess.mu.Lock()
	sess.tools, sess.toolsAt = tools, time.Now()
	sess.mu.Unlock()
	return tools, nil
}

// toolOf is a server's tool by name, from a fresh list when the cached one lacks it.
func (s *Service) toolOf(ctx context.Context, n named, name string) (tool, bool, error) {
	tools, err := s.toolsOf(ctx, n, false)
	if err != nil {
		return tool{}, false, err
	}
	if t, ok := tools[name]; ok {
		return t, true, nil
	}
	if tools, err = s.toolsOf(ctx, n, true); err != nil {
		return tool{}, false, err
	}
	t, ok := tools[name]
	return t, ok, nil
}
