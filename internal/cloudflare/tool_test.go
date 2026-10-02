package cloudflare

import (
	"strings"
	"testing"

	"github.com/repogo/host/internal/clilogin"
)

// The shape of `wrangler login --browser=false` in wrangler 4.146, read from
// its source (10-01); the page is an example.
const loginOutput = `
Attempting to login via OAuth...
Visit this link to authenticate: https://dash.cloudflare.com/oauth2/auth?response_type=code&client_id=x&redirect_uri=http%3A%2F%2Flocalhost%3A8976%2Foauth%2Fcallback&state=s
`

func TestLoginLineReadsThePageAndForwardsItsCallback(t *testing.T) {
	var c clilogin.Code
	done := false
	for _, line := range strings.Split(loginOutput, "\n") {
		if done = loginLine(line, &c); done {
			break
		}
	}
	if !done || !strings.HasPrefix(c.URL, "https://dash.cloudflare.com/oauth2/auth?") || c.UserCode != "" {
		t.Fatalf("code = %+v, done %v", c, done)
	}
}

// forward.fetch reaches only 127.0.0.1, so wrangler's callback server must
// bind there rather than whatever localhost resolves to.
func TestToolBindsTheCallbackWhereForwardReaches(t *testing.T) {
	tool := Tool()
	if tool.Kind != "cloudflare" || tool.Category != "cloud" || tool.Spec.Command != "wrangler" || tool.Spec.Pkg != "wrangler" {
		t.Fatalf("tool = %+v", tool)
	}
	if !strings.Contains(strings.Join(tool.Spec.LoginArgs, " "), "--callback-host=127.0.0.1") || tool.Spec.LoginStyle != clilogin.StyleCallback {
		t.Fatalf("login args %v", tool.Spec.LoginArgs)
	}
}
