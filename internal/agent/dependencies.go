package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Identity is required; every other interface describes an optional capability.
type Identity interface {
	Kind() Kind
	Name() string
}

// Dependencies are scoped to one host. Root confines provider files in tests.
type Dependencies struct {
	Context context.Context

	// Root, when set, holds every provider's home instead of the user's own
	// config directories, so a test host never reads a real account.
	Root string

	// Env is added to every agent process the host spawns.
	Env []string

	// MCP is the servers a turn in cwd gets; called only while a turn starts,
	// so a host that never runs one may leave it nil.
	MCP func(context.Context, string) []MCPServer

	// Tools is RepoGo's own MCP server; nil on a host that serves none.
	Tools Tools

	Log *slog.Logger
}

// MCPServer is one remote MCP server a turn gets. HTTP only: the servers
// RepoGo connects are remote, and an agent's own stdio servers load from its
// own config. Each adapter writes it in its CLI's config shape.
type MCPServer struct {
	Type    string   `json:"type"` // "http"
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Headers []Header `json:"headers"`
}

type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ConfigHome is an agent's config directory: the env override when set, else
// dir under the user's home.
func ConfigHome(env, dir string) string {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, dir)
}
