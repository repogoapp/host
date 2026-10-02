package agent

import (
	"context"

	"github.com/repogo/host/internal/device"
)

// ToolsServer is the name RepoGo's own MCP server goes by in a session, and so
// the namespace of its tools: mcp__repogo__browser to Claude, mcp.repogo.browser
// to Codex.
const ToolsServer = "repogo"

// Tools is RepoGo's own MCP server, served by this host. A session opened in a
// project it is switched on for gets an entry that names that session, so a
// call can be traced to the turn running there.
type Tools interface {
	On(cwd string) bool
	// Attach is the entry for one session and what to call when it closes.
	Attach(ToolSession) (MCPServer, func())
}

// ToolSession is one agent process as the tools see it.
type ToolSession interface {
	// Running is the turn the process is on now; false between turns.
	Running() (RunningTurn, bool)
}

// RunningTurn is what a tool needs to act for a turn.
type RunningTurn struct {
	// Ends with the turn, so a tool waiting on a phone stops when it is stopped.
	Ctx    context.Context
	TurnID string
	ChatID string
	Cwd    string
	// Device sent the turn; empty for one started on the host itself.
	Device device.ID
}
