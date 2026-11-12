// Package claudecode speaks Claude Code's stream-json protocol over stdio.
package claudecode

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// promptFile is whether the appended prompt goes by path: on Windows only.
var promptFile = runtime.GOOS == "windows"

// files are what args name by path, in a folder only this user can read.
type files struct{ mcpConfig, prompt string }

// args names the MCP config by path: inline, its headers (API keys, OAuth
// tokens) would sit in the process list for anyone running ps. On Windows the
// prompt goes by path too, as it overruns the 32,767-character command line;
// elsewhere it stays inline, since Claude Code before 2 lacks the file flag.
func (o Options) args(f files) ([]string, error) {
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
		a = append(a, "--setting-sources="+strings.Join(o.SettingSources, ","))
	}
	switch {
	case f.prompt != "":
		a = append(a, "--append-system-prompt-file", f.prompt)
	case o.AppendSystemPrompt != "":
		a = append(a, "--append-system-prompt", o.AppendSystemPrompt)
	}
	if o.AllowBypass {
		a = append(a, "--allow-dangerously-skip-permissions")
	}
	if f.mcpConfig != "" {
		a = append(a, "--mcp-config", f.mcpConfig)
	}
	return a, nil
}

// writeFiles writes the MCP servers and the appended prompt to files only this
// user can read, in a folder of their own; remove deletes it. With neither it
// writes nothing.
func (o Options) writeFiles() (f files, remove func(), err error) {
	if len(o.MCPServers) == 0 && (o.AppendSystemPrompt == "" || !promptFile) {
		return files{}, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "repogo-claude-")
	if err != nil {
		return files{}, nil, err
	}
	remove = func() { _ = os.RemoveAll(dir) }
	write := func(name string, b []byte) (string, error) {
		path := filepath.Join(dir, name)
		return path, os.WriteFile(path, b, 0o600)
	}
	if len(o.MCPServers) > 0 {
		b, err := json.Marshal(struct {
			Servers map[string]MCPServer `json:"mcpServers"`
		}{o.MCPServers})
		if err == nil {
			f.mcpConfig, err = write("mcp.json", b)
		}
		if err != nil {
			remove()
			return files{}, nil, err
		}
	}
	if o.AppendSystemPrompt != "" && promptFile {
		if f.prompt, err = write("prompt.md", []byte(o.AppendSystemPrompt)); err != nil {
			remove()
			return files{}, nil, err
		}
	}
	return f, remove, nil
}
