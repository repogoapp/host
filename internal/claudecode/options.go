// Package claudecode speaks Claude Code's stream-json protocol over stdio.
package claudecode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type MCPServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

type Options struct {
	Executable, Cwd       string
	Env                   []string
	SessionID, ResumeID   string
	Model, PermissionMode string
	SettingSources        []string
	AllowBypass           bool
	MCPServers            map[string]MCPServer
	// AppendSystemPrompt is added after Claude Code's own system prompt.
	AppendSystemPrompt string
}

// args names the MCP config by path: written inline, its headers (API keys,
// OAuth tokens) would sit in the process list for anyone running ps.
func (o Options) args(mcpConfig string) ([]string, error) {
	if o.Executable == "" || o.Cwd == "" {
		return nil, errors.New("claudecode: executable and cwd are required")
	}
	if o.SessionID != "" && o.ResumeID != "" {
		return nil, errors.New("claudecode: session and resume are mutually exclusive")
	}
	a := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--include-partial-messages", "--permission-prompt-tool", "stdio", "--replay-user-messages"}
	for _, pair := range [][2]string{{"--session-id", o.SessionID}, {"--resume", o.ResumeID}, {"--model", o.Model}, {"--permission-mode", o.PermissionMode}} {
		if pair[1] != "" {
			a = append(a, pair[0], pair[1])
		}
	}
	if o.SettingSources != nil {
		sources := ""
		for i, source := range o.SettingSources {
			if i > 0 {
				sources += ","
			}
			sources += source
		}
		a = append(a, "--setting-sources="+sources)
	}
	if o.AppendSystemPrompt != "" {
		a = append(a, "--append-system-prompt", o.AppendSystemPrompt)
	}
	if o.AllowBypass {
		a = append(a, "--allow-dangerously-skip-permissions")
	}
	if mcpConfig != "" {
		a = append(a, "--mcp-config", mcpConfig)
	}
	return a, nil
}

// writeMCPConfig writes the servers to a file only this user can read, in a
// folder of its own; remove deletes both. No servers writes nothing.
func (o Options) writeMCPConfig() (path string, remove func(), err error) {
	if len(o.MCPServers) == 0 {
		return "", func() {}, nil
	}
	b, err := json.Marshal(struct {
		Servers map[string]MCPServer `json:"mcpServers"`
	}{o.MCPServers})
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("", "repogo-claude-")
	if err != nil {
		return "", nil, err
	}
	remove = func() { _ = os.RemoveAll(dir) }
	path = filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		remove()
		return "", nil, err
	}
	return path, remove, nil
}
