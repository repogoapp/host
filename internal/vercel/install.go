package vercel

import (
	"cmp"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/repogo/host/internal/clilogin"
	"github.com/repogo/host/internal/clitool"
)

// Tool is the Vercel CLI: found on PATH, installed and updated with npm or
// brew, signed in with `vercel login`.
func Tool() clitool.Tool {
	return clitool.Tool{
		Kind:         "vercel",
		Name:         "Vercel",
		Icon:         "vercel",
		Category:     clitool.CategoryCloud,
		Capabilities: []string{clitool.CapabilityInstall, clitool.CapabilityUpdate, clitool.CapabilityLogin},
		Spec: clitool.Spec{
			Command:   "vercel",
			Pkg:       "vercel",
			Formula:   "vercel-cli",
			AuthProbe: whoami,
			// Before 45 the CLI asks for a provider instead of printing the
			// device link unless given --future; later ones accept it with a warning.
			LoginArgs: []string{"login", "--future"},
			// CI keeps the CLI from opening a browser on the environment, where
			// nobody is looking; the phone opens the link instead.
			LoginEnv:   []string{"CI=1", "NO_COLOR=1"},
			LoginRead:  loginLine,
			LogoutArgs: []string{"logout"},
		},
	}
}

// whoami asks the CLI who it is signed in as. Its token file outlives a
// revoked token, so only Vercel's answer says the sign-in still works.
func whoami(ctx context.Context, path string) (bool, string, *clitool.Account) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if out, err := runWhoami(ctx, path, "--format", "json", "--non-interactive"); err == nil {
		return readWhoamiJSON(out)
	}
	// CLIs before 45 reject --format and --non-interactive and print only
	// the username; CI keeps them from prompting to sign in.
	out, err := runWhoami(ctx, path)
	if err != nil {
		return false, "", nil
	}
	return readWhoamiLine(out)
}

func runWhoami(ctx context.Context, path string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, append([]string{"whoami"}, args...)...)
	cmd.Dir = os.TempDir()
	cmd.Env = append(os.Environ(), "CI=1", "NO_COLOR=1")
	cmd.WaitDelay = time.Second
	return cmd.Output()
}

func readWhoamiJSON(out []byte) (bool, string, *clitool.Account) {
	var me struct {
		Username string `json:"username"`
		Email    string `json:"email"`
		Name     string `json:"name"`
	}
	if json.Unmarshal(out, &me) != nil || me.Username == "" {
		return false, "", nil
	}
	return true, "vercel whoami", &clitool.Account{Email: me.Email, Name: cmp.Or(me.Name, me.Username), Login: me.Username}
}

func readWhoamiLine(out []byte) (bool, string, *clitool.Account) {
	words := strings.Fields(strings.TrimSpace(string(out)))
	if len(words) != 1 {
		return false, "", nil
	}
	return true, "vercel whoami", &clitool.Account{Name: words[0], Login: words[0]}
}

var loginURL = regexp.MustCompile(`https://\S*vercel\.com/\S+`)

// loginLine reads the device page `vercel login` prints ("Visit <url>"). The
// URL already carries the code; it is shown so the person can match it.
func loginLine(line string, c *clilogin.Code) bool {
	link := loginURL.FindString(line)
	if link == "" {
		return false
	}
	c.URL = link
	if parsed, err := url.Parse(link); err == nil {
		c.UserCode = parsed.Query().Get("user_code")
	}
	return true
}
