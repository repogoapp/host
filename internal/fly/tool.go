// Package fly is Fly.io's flyctl CLI on this host.
package fly

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

// Tool is flyctl: installed with Fly's script into ~/.fly/bin, which updates
// itself, or with brew; signed in through a page flyctl polls for.
func Tool() clitool.Tool {
	return clitool.Tool{
		Kind:         "fly",
		Name:         "Fly.io",
		Icon:         "fly",
		Category:     clitool.CategoryCloud,
		Capabilities: []string{clitool.CapabilityInstall, clitool.CapabilityUpdate, clitool.CapabilityLogin},
		Spec: clitool.Spec{
			Command:       "flyctl",
			ReleaseRepo:   "superfly/flyctl",
			Formula:       "flyctl",
			InstallScript: "curl -fsSL https://fly.io/install.sh | sh",
			NativePaths:   []string{".fly/bin/flyctl"},
			NativeUpdate:  []string{"flyctl", "version", "upgrade"},
			AuthProbe:     whoami,
			LoginArgs:     []string{"auth", "login"},
			LoginEnv:      []string{"FLY_NO_UPDATE_CHECK=1", "NO_COLOR=1"},
			LoginRead:     loginLine,
			// flyctl polls until the page is approved, and also takes the
			// one-time code the page shows.
			LoginStyle: clilogin.StylePaste,
			// `fly auth login` refuses to start without a terminal.
			LoginTTY:   true,
			LogoutArgs: []string{"auth", "logout"},
		},
	}
}

// whoami asks Fly who flyctl is signed in as; it exits non-zero when signed out.
func whoami(ctx context.Context, path string) (bool, string, *clitool.Account) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "auth", "whoami", "--json")
	cmd.Dir = os.TempDir()
	cmd.Env = append(cmd.Environ(), "FLY_NO_UPDATE_CHECK=1")
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		return false, "", nil
	}
	var me struct {
		Email string `json:"email"`
	}
	if json.Unmarshal(out, &me) != nil || me.Email == "" {
		return false, "", nil
	}
	return true, "flyctl auth whoami", &clitool.Account{Email: me.Email}
}

var signInURL = regexp.MustCompile(`https://[^\s()]*fly\.io/[^\s()]+`)

// loginLine reads the page flyctl prints, "Opening <url> ..." or, when it
// cannot open a browser, "Copy the url (<url>)".
func loginLine(line string, c *clilogin.Code) bool {
	c.URL = signInURL.FindString(line)
	return c.URL != ""
}
