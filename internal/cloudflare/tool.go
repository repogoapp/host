// Package cloudflare is Cloudflare's wrangler CLI on this host.
package cloudflare

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"time"

	"github.com/repogo/host/internal/clilogin"
	"github.com/repogo/host/internal/clitool"
)

// Tool is wrangler: installed and updated with npm or brew. Its sign-in page
// redirects to a callback server wrangler runs on localhost, which the phone
// reaches through forward.fetch; that dials 127.0.0.1, so the server binds there.
func Tool() clitool.Tool {
	return clitool.Tool{
		Kind:         "cloudflare",
		Name:         "Cloudflare",
		Icon:         "cloudflare",
		Category:     clitool.CategoryCloud,
		Capabilities: []string{clitool.CapabilityInstall, clitool.CapabilityUpdate, clitool.CapabilityLogin},
		Spec: clitool.Spec{
			Command:    "wrangler",
			Pkg:        "wrangler",
			Formula:    "cloudflare-wrangler",
			AuthProbe:  signedIn,
			LoginArgs:  []string{"login", "--browser=false", "--callback-host=127.0.0.1"},
			LoginEnv:   []string{"NO_COLOR=1", quiet},
			LoginRead:  loginLine,
			LoginStyle: clilogin.StyleCallback,
			// The page always redirects to localhost:8976/oauth/callback.
			LoginPort:  8976,
			LogoutArgs: []string{"logout"},
		},
	}
}

// quiet keeps wrangler from posting usage telemetry, which holds the process
// open for 10 s or more after its work is done.
var quiet = "WRANGLER_SEND_METRICS=false"

// signedIn asks wrangler for its token, which it reads from its own config
// (refreshing an expired one): 0.4 s, where `whoami` takes 4 s to a minute.
// The token itself is only checked for presence, never kept.
func signedIn(ctx context.Context, path string) (bool, string, *clitool.Account) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "auth", "token", "--json")
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), quiet)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return false, "", nil
	}
	var auth struct {
		Token string `json:"token"`
		Key   string `json:"key"`
	}
	if json.Unmarshal(out, &auth) != nil || (auth.Token == "" && auth.Key == "") {
		return false, "", nil
	}
	return true, "wrangler auth token", nil
}

var authorizeURL = regexp.MustCompile(`Visit this link to authenticate: (https://\S+)`)

// loginLine reads the page `wrangler login --browser=false` prints. Approving
// it redirects to wrangler's callback, which the phone forwards.
func loginLine(line string, c *clilogin.Code) bool {
	m := authorizeURL.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	c.URL = m[1]
	return true
}
