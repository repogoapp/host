package agents_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogo/host/internal/agent"
	claudeprovider "github.com/repogo/host/internal/agents/claude"
	codexprovider "github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/mcp"
)

func TestInstalledListsAgentServersWithoutTheirSecrets(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(home, "claude", ".claude.json"), `{
	  "mcpServers": {
	    "github": {"command": "/usr/local/bin/npx", "args": ["-y", "@modelcontextprotocol/server-github"], "env": {"GITHUB_TOKEN": "ghp_secret"}},
	    "sentry": {"type": "http", "url": "https://mcp.sentry.dev/mcp?token=abc", "headers": {"Authorization": "Bearer xyz"}}
	  },
	  "projects": {"`+project+`": {"mcpServers": {"local-db": {"command": "db-mcp"}}}}
	}`)
	writeFile(t, filepath.Join(project, ".mcp.json"), `{"mcpServers": {"figma": {"type": "sse", "url": "http://127.0.0.1:3845/sse"}}}`)
	writeFile(t, filepath.Join(home, "codex", "config.toml"), `model = "gpt-5"

[mcp_servers.context7]
command = "npx"
args = ["-y", "@upstash/context7-mcp"]

[mcp_servers.context7.env]
API_KEY = "c7-secret"

[mcp_servers."linear app"]
url = "https://mcp.linear.app/mcp"

[mcp_servers.off]
command = "off"
enabled = false
`)

	s, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"), func(mcp.Changed) {}, claudeprovider.New(agent.Dependencies{Root: home}), codexprovider.New(agent.Dependencies{Root: home}))
	if err != nil {
		t.Fatal(err)
	}
	got := s.List(project).Installed
	want := []mcp.Installed{
		{Name: "figma", Agent: "claude", Scope: "project", Transport: "sse", Target: "127.0.0.1:3845/sse"},
		{Name: "github", Agent: "claude", Scope: "user", Transport: "stdio", Target: "npx @modelcontextprotocol/server-github"},
		{Name: "local-db", Agent: "claude", Scope: "local", Transport: "stdio", Target: "db-mcp"},
		{Name: "sentry", Agent: "claude", Scope: "user", Transport: "http", Target: "mcp.sentry.dev/mcp"},
		{Name: "context7", Agent: "codex", Scope: "user", Transport: "stdio", Target: "npx @upstash/context7-mcp"},
		{Name: "linear app", Agent: "codex", Scope: "user", Transport: "http", Target: "mcp.linear.app/mcp"},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("installed =\n%+v\nwant\n%+v", got, want)
	}
	b, _ := json.Marshal(got)
	for _, secret := range []string{"ghp_secret", "abc", "xyz", "c7-secret"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("installed list leaks %q: %s", secret, b)
		}
	}
	if user := s.List("").Installed; len(user) != 4 {
		t.Fatalf("without a project want the 4 user-scope servers, got %+v", user)
	}
}
