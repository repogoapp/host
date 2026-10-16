// Package clientprompt is what a session adds to the agent's system prompt
// for the client that started it: how to write for what that client renders.
package clientprompt

import (
	_ "embed"

	"github.com/repogoapp/host/internal/device"
)

//go:embed markdown_v1.md
var markdownV1 string

// cards.md is the phone's own card prompt, copied by this package's test
// with -update, so the two never drift.
//
//go:embed cards.md
var cards string

// cards_plugins.md is the phone's prompt for cards that read and write a
// project's plugins, copied the same way.
//
//go:embed cards_plugins.md
var cardsPlugins string

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

// For is the prompt for c, or "" when it renders nothing beyond plain markdown;
// a newer markdown version gets the newest text. A client whose cards reach
// plugins also learns to build them, and to draw the ones the project has.
func For(c device.Client, plugins Plugins) string {
	if c.Features["markdown"] < 1 {
		return ""
	}
	prompt := markdownV1 + "\n" + cards
	if c.Features["plugins"] < 1 || plugins == NoPlugins {
		return prompt
	}
	prompt += "\n" + apps
	if plugins == HasPlugins {
		prompt += "\n" + cardsPlugins
	}
	return prompt
}
