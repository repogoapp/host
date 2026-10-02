package claude

import (
	"regexp"

	"github.com/repogo/host/internal/clilogin"
)

// Claude prints one authorize link and waits for the code its page shows.
var claudeAuthorizeURL = regexp.MustCompile(`https://\S+/oauth/authorize\?\S+`)

// claudeLoginLine reads `claude auth login`'s output for the authorize link;
// the phone opens it and pastes back the code the page shows.
func claudeLoginLine(line string, c *clilogin.Code) bool {
	if u := claudeAuthorizeURL.FindString(line); u != "" {
		c.URL = u
	}
	return c.URL != ""
}
