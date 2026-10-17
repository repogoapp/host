package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Answer is a tool's result as a card reads it, or its refusal as data: the
// shape plugins.call answers in, so a card treats both alike.
type Answer struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Failure        `json:"error,omitempty"`
}

// Failure is a refusal to show: CONFIRM (ask, then call again confirmed),
// NOT_READ_ONLY (a read naming a tool that writes) or TOOL_ERROR.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Call runs one tool of a server switched on for project, named by the key
// an agent knows it by. A read runs only read-only tools; a destructive tool
// answers CONFIRM until confirmed says the user agreed.
func (s *Service) Call(ctx context.Context, project, server, toolName string, input json.RawMessage, read, confirmed bool) (Answer, error) {
	n, err := s.named(ctx, project, server)
	if err != nil {
		return Answer{}, err
	}
	t, ok, err := s.toolOf(ctx, n, toolName)
	if err != nil {
		return Answer{}, err
	}
	switch {
	case !ok:
		return Answer{}, ErrNotFound.Errorf("%s has no tool %s", n.record.Label, toolName)
	case read && !t.readOnly():
		return refusal("NOT_READ_ONLY", fmt.Sprintf("%s changes data, so a card runs it from a Button, not a Data", toolName)), nil
	case !read && t.destructive() && !confirmed:
		return refusal("CONFIRM", fmt.Sprintf("Run %s on %s?", t.label(), n.record.Label)), nil
	}
	if len(bytes.TrimSpace(input)) == 0 {
		input = json.RawMessage("{}")
	}
	raw, err := s.request(ctx, n, "tools/call", map[string]any{"name": toolName, "arguments": input})
	var refused *rpcError
	if errors.As(err, &refused) {
		return refusal("TOOL_ERROR", refused.Message), nil
	}
	if err != nil {
		return Answer{}, err
	}
	return answerOf(raw)
}

// named is project's server under its key, as onFor names it.
func (s *Service) named(ctx context.Context, project, server string) (named, error) {
	for _, n := range s.onFor(ctx, project) {
		if n.key == server {
			return n, nil
		}
	}
	return named{}, ErrNotFound.Errorf("no MCP server %q is on for this project; connect it or switch it on in RepoGo's MCP settings", server)
}

// answerOf is a tool's result: its structured content when it has some, else
// its text, as JSON when the text is JSON and as a string when it is not.
func answerOf(raw json.RawMessage) (Answer, error) {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
		IsError           bool            `json:"isError"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return Answer{}, fmt.Errorf("mcp: unreadable tool result: %w", err)
	}
	var texts []string
	for _, c := range result.Content {
		if c.Type == "text" {
			texts = append(texts, c.Text)
		}
	}
	text := strings.TrimSpace(strings.Join(texts, "\n"))
	if result.IsError {
		if text == "" {
			text = "The tool failed."
		}
		return refusal("TOOL_ERROR", text), nil
	}
	if s := bytes.TrimSpace(result.StructuredContent); len(s) > 0 && !bytes.Equal(s, []byte("null")) {
		return Answer{Result: s}, nil
	}
	if text != "" && json.Valid([]byte(text)) {
		return Answer{Result: json.RawMessage(text)}, nil
	}
	quoted, err := json.Marshal(text)
	return Answer{Result: quoted}, err
}

func refusal(code, message string) Answer {
	return Answer{Error: &Failure{Code: code, Message: message}}
}

// ServerTools is a server switched on for a project and its tools, as an agent
// writing a card needs them; Error is why its tools couldn't be listed.
type ServerTools struct {
	Server string     `json:"server"`
	Label  string     `json:"label"`
	Tools  []ToolInfo `json:"tools"`
	Error  string     `json:"error,omitempty"`
}

// ToolInfo is one tool: only a read-only one feeds a card's Data, and one that
// asks first gets the user's yes before a Button runs it.
type ToolInfo struct {
	Name      string `json:"name"`
	ReadOnly  bool   `json:"read_only"`
	AsksFirst bool   `json:"asks_first"`
}

// Tools lists the tools of every server switched on for cwd's project, by
// name. A server that can't be reached says why instead of failing the rest.
func (s *Service) Tools(ctx context.Context, cwd string) []ServerTools {
	out := []ServerTools{}
	for _, n := range s.onFor(ctx, cwd) {
		entry := ServerTools{Server: n.key, Label: n.record.Label, Tools: []ToolInfo{}}
		tools, err := s.toolsOf(ctx, n, false)
		if err != nil {
			entry.Error = err.Error()
		}
		for _, t := range tools {
			entry.Tools = append(entry.Tools, ToolInfo{Name: t.Name, ReadOnly: t.readOnly(), AsksFirst: t.destructive()})
		}
		slices.SortFunc(entry.Tools, func(a, b ToolInfo) int { return strings.Compare(a.Name, b.Name) })
		out = append(out, entry)
	}
	return out
}
