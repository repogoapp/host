package claude

import (
	"context"
	"encoding/json"
	"os/exec"
	"time"

	"github.com/repogoapp/host/internal/clitool"
)

// authStatusTimeout bounds `claude auth status`, which answers in about 0.1 s.
const authStatusTimeout = 5 * time.Second

// authStatus asks Claude, with its turns' environment, whether it is signed in:
// a token that failed to refresh leaves its Keychain item behind. With no
// readable answer (a build without `auth status`) it checks the disk instead.
func (p *Provider) authStatus(ctx context.Context, path string) (bool, string, *clitool.Account) {
	ctx, cancel := context.WithTimeout(ctx, authStatusTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "auth", "status", "--json")
	cmd.Env = p.environment()
	// Signed out exits 1 with the same JSON, so the exit code is not the answer.
	out, _ := cmd.Output()
	if loggedIn, method, ok := parseAuthStatus(out); ok {
		if !loggedIn {
			return false, "", nil
		}
		return true, "claude auth status:" + method, nil
	}
	s := definition().Tool.Spec
	authed, source := clitool.ProbeAuth(ctx, s, p.home)
	return authed, source, nil
}

// parseAuthStatus reads `claude auth status --json`; ok is false when the
// output is not that JSON.
func parseAuthStatus(out []byte) (loggedIn bool, method string, ok bool) {
	var status struct {
		LoggedIn   *bool  `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if json.Unmarshal(out, &status) != nil || status.LoggedIn == nil {
		return false, "", false
	}
	return *status.LoggedIn, status.AuthMethod, true
}
