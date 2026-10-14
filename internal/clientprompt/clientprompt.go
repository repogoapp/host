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

// For is the prompt for c, or "" when it renders nothing beyond plain
// markdown. A newer markdown version than the host knows gets the newest text.
func For(c device.Client) string {
	if c.Features["markdown"] >= 1 {
		return markdownV1 + "\n" + cards
	}
	return ""
}
