// Package clientprompt is what a session adds to the agent's system prompt
// for the client that started it: how to write for what that client renders.
package clientprompt

import (
	_ "embed"
	"strings"

	"github.com/repogoapp/host/internal/device"
)

//go:embed markdown_v1.md
var markdownV1 string

// cards.md is the phone's own card prompt, copied by this package's test
// with -update, so the two never drift.
//
//go:embed cards.md
var cards string

// cards_data.md is the elements cards read and write live data with, and
// cards_plugins.md, cards_mcp.md and cards_repogo.md its sources; copied the same way.
//
//go:embed cards_data.md
var cardsData string

//go:embed cards_plugins.md
var cardsPlugins string

//go:embed cards_mcp.md
var cardsMCP string

//go:embed cards_repogo.md
var cardsRepoGo string

// apps.md is the phone's prompt for building a project's tabs and plugins,
// copied the same way.
//
//go:embed apps.md
var apps string

// Plugins is what a session's project can do with plugins.
type Plugins int

const (
	// NoPlugins: the environment can't run plugins.
	NoPlugins Plugins = iota
	// CanAddPlugins: the environment runs plugins; the project has none yet.
	CanAddPlugins
	// HasPlugins: the project has a .repogo/plugins folder.
	HasPlugins
)

// For is the prompt for c, or "" when it renders nothing beyond plain markdown.
// A client whose cards reach plugins learns to build them and draw the project's;
// one whose cards reach MCP servers, to draw those on for it; one with repogo, the chats.
func For(c device.Client, plugins Plugins, servers []string) string {
	if c.Features["markdown"] < 1 {
		return ""
	}
	prompt := markdownV1 + "\n" + cards
	pluginCards := c.Features["plugins"] >= 1 && plugins != NoPlugins
	mcpCards := c.Features["mcp"] >= 1 && len(servers) > 0
	hostCards := c.Features["repogo"] >= 1
	if pluginCards {
		prompt += "\n" + apps
	}
	if (pluginCards && plugins == HasPlugins) || mcpCards || hostCards {
		prompt += "\n" + cardsData
	}
	if pluginCards && plugins == HasPlugins {
		prompt += "\n" + cardsPlugins
	}
	if hostCards {
		prompt += "\n" + cardsRepoGo
	}
	// Last, so the prompt ends naming the servers.
	if mcpCards {
		prompt += "\n" + cardsMCP + "MCP servers for cards: `" + strings.Join(servers, "`, `") + "`.\n"
	}
	return prompt
}
