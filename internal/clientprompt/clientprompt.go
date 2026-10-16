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

// For is the prompt for c, or "" when it renders nothing beyond plain markdown;
// a newer markdown version gets the newest text. withPlugins adds live-data
// cards, for a project with plugins and a client whose cards reach them.
func For(c device.Client, withPlugins bool) string {
	if c.Features["markdown"] < 1 {
		return ""
	}
	prompt := markdownV1 + "\n" + cards
	if withPlugins && c.Features["plugins"] >= 1 {
		prompt += "\n" + cardsPlugins
	}
	return prompt
}
