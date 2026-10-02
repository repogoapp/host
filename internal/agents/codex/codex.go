// Package codex owns OpenAI Codex: its install definition, app-server runner,
// rollout parsing, usage, hooks, MCP configuration and home resolution.
package codex

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/binfetch"
	"github.com/repogo/host/internal/clitool"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/session"
)

// definition is Codex's descriptor and install configuration, without the
// per-host paths New fills in.
func definition() agent.Provider {
	return agent.Provider{
		Company:    "openai",
		ResumeArgs: func(sessionID string) []string { return []string{"resume", sessionID} },
		Tool: clitool.Tool{
			Kind:     string(agent.KindCodex),
			Name:     "Codex",
			Icon:     "agent-codex",
			Category: clitool.CategoryAgent,
			Spec: clitool.Spec{
				Command:   "codex",
				Pkg:       "@openai/codex",
				Formula:   "codex",
				AuthPaths: []string{"auth.json"},
				Account:   codexAccount,
				// Where codexRelease puts it; that build updates by fetching again.
				NativePaths: []string{".local/bin/codex"},
				Release:     codexRelease,
				// ChatGPT's device code, which Codex polls for like gh does.
				LoginArgs:  []string{"login", "--device-auth"},
				LoginEnv:   []string{"NO_COLOR=1"},
				LoginRead:  codexLoginLine,
				LogoutArgs: []string{"logout"},
			},
		},
	}
}

func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}

// codexRelease is Codex's own build for a system: one binary named after its
// target, in codex-<target>.tar.gz on the latest release.
func codexRelease(_ context.Context, goos, goarch string) (string, string, error) {
	arch := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[goarch]
	system := map[string]string{"linux": "unknown-linux-musl", "darwin": "apple-darwin"}[goos]
	if arch == "" || system == "" {
		return "", "", fmt.Errorf("%w: codex on %s/%s", binfetch.ErrUnsupported, goos, goarch)
	}
	name := "codex-" + arch + "-" + system
	return binfetch.Base + "/openai/codex/releases/latest/download/" + name + ".tar.gz", name, nil
}

// codexAccount reads the email and account claims of the ChatGPT id_token in auth.json.
// Decoded, not verified: it names the account on a screen and grants nothing.
func codexAccount(path string) *clitool.Account {
	var file struct {
		Tokens struct {
			IDToken string `json:"id_token"`
		} `json:"tokens"`
	}
	if !clitool.ReadJSON(path, &file) {
		return nil
	}
	parts := strings.Split(file.Tokens.IDToken, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil
	}
	var claims struct {
		Email string `json:"email"`
		Name  string `json:"name"`
		// The account id is the workspace on Team plans, so the user id too.
		Auth struct {
			User    string `json:"chatgpt_user_id"`
			Account string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Email == "" {
		return nil
	}
	return &clitool.Account{Email: claims.Email, Name: claims.Name,
		Key: clitool.AccountKey(string(agent.KindCodex), claims.Auth.User, claims.Auth.Account)}
}

// Provider owns this host's runner and provider-specific capabilities.
type Provider struct {
	*runner
	sessions          *Sessions
	home, accountPath string
}

// New builds this host's Codex provider. A Root in deps (tests, the hermetic
// test host) puts CODEX_HOME under it instead of the user's own.
func New(deps agent.Dependencies) *Provider {
	home := defaultHome()
	if deps.Root != "" {
		home = filepath.Join(deps.Root, string(agent.KindCodex))
	}
	account := filepath.Join(home, "auth.json")
	return &Provider{
		runner: &runner{deps: deps, executable: executable, home: home},
		home:   home, accountPath: account, sessions: NewSessions(home),
	}
}

// Name is what every screen calls it: "Codex".
func (p *Provider) Name() string { return definition().Tool.Name }

// Definition is the install configuration with this host's home and account
// paths, which the shared inventory installs, updates and signs in with.
func (p *Provider) Definition() agent.Provider {
	d := definition()
	d.Tool.Home = p.home
	d.Tool.AccountPath = p.accountPath
	return d
}

// Sessions reads this host's Codex rollouts.
func (p *Provider) Sessions() session.Provider { return p.sessions }

// Hooks wires the helper into hooks.json in this host's Codex home.
func (p *Provider) Hooks() notify.Provider { return Hooks{Home: p.home} }
