package mcp

import (
	"net/url"
	"path/filepath"
	"sort"
	"strings"
)

// Installed is a server the user configured in an agent themselves. Shown
// read-only: the agent loads it on its own, so there is nothing to apply, and
// only the name and where it points cross the wire, never its env or headers.
type Installed struct {
	Name  string `json:"name"`
	Agent string `json:"agent"` // claude | codex
	// user: every project; project: the project's own file; local: Claude's
	// per-project entry in ~/.claude.json.
	Scope     string `json:"scope"`
	Transport string `json:"transport"` // stdio | http | sse
	// The URL without its query, or the command's name and package.
	Target string `json:"target"`
}

// installed lists what each agent already loads, for the project at root when
// one is given; a file that is missing or unreadable contributes nothing.
func (s *Service) installed(root string) []Installed {
	out := []Installed{}
	for _, p := range s.providers {
		out = append(out, p.InstalledMCP(root)...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// Target says where a server points without what it carries: a URL loses its
// query and userinfo, which is where keys go, and a command keeps its name
// and first plain argument, the package a runner like npx starts.
func Target(rawURL, command string, args []string) string {
	if rawURL != "" {
		u, err := url.Parse(rawURL)
		if err != nil || u.Host == "" {
			return ""
		}
		return u.Host + strings.TrimSuffix(u.Path, "/")
	}
	if command == "" {
		return ""
	}
	out := filepath.Base(command)
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return out + " " + a
		}
	}
	return out
}

// Provider is an agent whose own MCP configuration can be listed. project is
// the repository root, or empty for the user-level servers only.
type Provider interface {
	InstalledMCP(project string) []Installed
}
