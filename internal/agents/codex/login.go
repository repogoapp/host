package codex

import (
	"regexp"

	"github.com/repogo/host/internal/clilogin"
)

var (
	// Codex prints the page, then the code, each on its own line.
	codexDeviceURL  = regexp.MustCompile(`https://\S+/device\b\S*`)
	codexDeviceCode = regexp.MustCompile(`^[A-Z0-9]{4,}-[A-Z0-9]{4,}$`)
)

// codexLoginLine reads `codex login --device-auth`'s output until it has both
// the page and the code to enter there.
func codexLoginLine(line string, c *clilogin.Code) bool {
	if u := codexDeviceURL.FindString(line); u != "" {
		c.URL = u
	}
	if codexDeviceCode.MatchString(line) {
		c.UserCode = line
	}
	return c.URL != "" && c.UserCode != ""
}
