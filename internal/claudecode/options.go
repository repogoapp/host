// Package claudecode speaks Claude Code's stream-json protocol over stdio.
package claudecode

import (
	"encoding/json"
	"errors"
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
}

func (o Options) args() ([]string, error) {
	if o.Executable == "" || o.Cwd == "" {
		return nil, errors.New("claudecode: executable and cwd are required")
	}
	if o.SessionID != "" && o.ResumeID != "" {
		return nil, errors.New("claudecode: session and resume are mutually exclusive")
	}
	a := []string{"--output-format", "stream-json", "--verbose", "--input-format", "stream-json",
		"--include-partial-messages", "--permission-prompt-tool", "stdio", "--replay-user-messages",
		// Newer models stream no readable thinking unless a summary is requested.
		"--thinking-display", "summarized"}
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
	if o.AllowBypass {
		a = append(a, "--allow-dangerously-skip-permissions")
	}
	if len(o.MCPServers) > 0 {
		b, err := json.Marshal(struct {
			Servers map[string]MCPServer `json:"mcpServers"`
		}{o.MCPServers})
		if err != nil {
			return nil, err
		}
		a = append(a, "--mcp-config", string(b))
	}
	return a, nil
}
