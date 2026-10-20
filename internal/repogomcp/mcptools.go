package repogomcp

import (
	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/mcp"
)

// The MCP servers the user connected in RepoGo and switched on for a project
// also feed its cards (`mcp:<server>.<tool>`); this tool tells an agent what
// each one offers a card.

const mcpToolsInstructions = " `mcp_tools` lists the tools of the MCP servers switched on for this project, " +
	"which cards can call."

var mcpToolsTool = tool{
	Name: "mcp_tools",
	Description: "List the MCP servers the user connected in RepoGo and switched on for this project, each by the " +
		"key a card names it with (`mcp:<server>.<tool>`), with its tools: `read_only` tools can feed a card's Data; " +
		"the rest run from a Button, and `asks_first` ones ask the user before running. " +
		"Call this before a card names a tool; call the tool once first only when you don't know its answer's shape.",
	InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
}

// withMCP reports whether a folder's project has servers a card can call.
func (s *Server) withMCP(cwd string) bool {
	return len(s.cfg.MCP().Keys(cwd)) > 0
}

func mcpTools(service *mcp.Service, turn agent.RunningTurn) toolResult {
	return succeeded(map[string]any{"servers": service.Tools(turn.Ctx, turn.Cwd, false)})
}
