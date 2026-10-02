package claude

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/mcp"
)

// claudeEntry is the part of a Claude server entry that is safe to show.
type claudeEntry struct {
	Type    string   `json:"type"`
	URL     string   `json:"url"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// claudeInstalled reads the account file: user-wide servers, plus the
// per-project ("local") entries for root when one is given.
func claudeInstalled(path, root string) []mcp.Installed {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		MCPServers map[string]claudeEntry `json:"mcpServers"`
		Projects   map[string]struct {
			MCPServers map[string]claudeEntry `json:"mcpServers"`
		} `json:"projects"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	out := claudeEntries(doc.MCPServers, "user")
	if root != "" {
		out = append(out, claudeEntries(doc.Projects[root].MCPServers, "local")...)
	}
	return out
}

// claudeFile reads a file that holds only mcpServers, such as a project's
// .mcp.json.
func claudeFile(path, scope string) []mcp.Installed {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		MCPServers map[string]claudeEntry `json:"mcpServers"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil
	}
	return claudeEntries(doc.MCPServers, scope)
}

// claudeEntries lists servers under scope. An entry with no type is stdio
// unless it names a URL.
func claudeEntries(servers map[string]claudeEntry, scope string) []mcp.Installed {
	var out []mcp.Installed
	for name, e := range servers {
		transport := e.Type
		if transport == "" || transport == "stdio" {
			transport = "stdio"
			if e.URL != "" {
				transport = "http"
			}
		}
		out = append(out, mcp.Installed{Name: name, Agent: string(agent.KindClaude), Scope: scope, Transport: transport, Target: mcp.Target(e.URL, e.Command, e.Args)})
	}
	return out
}

// InstalledMCP is what Claude already loads for root: the account file's
// user and local servers and the project's .mcp.json. A file that is missing
// or unreadable contributes nothing.
func (p *Provider) InstalledMCP(root string) []mcp.Installed {
	out := claudeInstalled(p.accountPath, root)
	if root != "" {
		out = append(out, claudeFile(filepath.Join(root, ".mcp.json"), "project")...)
	}
	return out
}
