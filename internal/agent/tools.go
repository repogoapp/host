package agent

import (
	"context"
	"encoding/json"

	"github.com/repogoapp/host/internal/device"
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
	// Attach is the entry for a session opened by req's turn and what to call
	// when it closes; its cwd and parent pick the tools it is offered.
	Attach(session ToolSession, req TurnRequest) (MCPServer, func())
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
	// What the turn runs with: the most a chat it starts or messages may get.
	Config TurnConfig
	// Parent is the chat whose agent started this one; empty for the user's.
	Parent string
	// Ask puts an approval on the turn's chat and blocks until the user answers,
	// as TurnIO.Ask does; a tool asks before acting outside the machine.
	Ask func(context.Context, Approval) (json.RawMessage, error)
}
