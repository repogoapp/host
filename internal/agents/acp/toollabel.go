package acp

import (
	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/chatwire"
	"github.com/repogoapp/host/internal/toollabel"
)

// ToolRow labels a call with the title the agent gave it ("Search Tesla
// news"), the one name every ACP agent sends; tools differ per agent, so
// only the icon is read from what the input names.
func (p *Provider) ToolRow(call agent.ToolCall) chatwire.Tool {
	if call.Name == "" {
		return chatwire.Tool{}
	}
	args, _ := toollabel.Input(call.Input)
	return chatwire.Labeled(call.Name, label(call.Name, args), args)
}

// Resolve is the call as recorded: an ACP call is always one tool.
func (p *Provider) Resolve(call agent.ToolCall) agent.ToolCall { return call }

func label(title string, args map[string]any) toollabel.Label {
	line := toollabel.Preview(title, 40, "tool")
	return toollabel.New(icon(args), line, line, line+" failed")
}

// icon is the SF Symbol for what a call's input names.
func icon(args map[string]any) string {
	switch {
	case toollabel.ShellCommand(args) != "":
		return "terminal"
	case toollabel.String(args, "query") != "":
		return "magnifyingglass"
	case toollabel.String(args, "url") != "":
		return "globe"
	case toollabel.String(args, "path") != "" || toollabel.String(args, "file_path") != "":
		return "doc.text"
	}
	return "wrench.and.screwdriver"
}
