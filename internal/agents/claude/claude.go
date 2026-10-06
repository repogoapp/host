// Package claude owns Claude Code: its install definition, direct runner,
// transcript parsing, usage, hooks, MCP configuration and home resolution.
package claude

import (
	"path/filepath"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/clilogin"
	"github.com/repogo/host/internal/clitool"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/session"
)

// keychainService is the macOS keychain service Claude Code keeps its login in.
const keychainService = "Claude Code-credentials"

// definition is Claude Code's descriptor and install configuration, without
// the per-host paths New fills in.
func definition() agent.Provider {
	return agent.Provider{
		Company:    "anthropic",
		ResumeArgs: func(sessionID string) []string { return []string{"--resume", sessionID} },
		Tool: clitool.Tool{
			Kind:     string(agent.KindClaude),
			Name:     "Claude",
			Icon:     "agent-claude",
			Category: clitool.CategoryAgent,
			Spec: clitool.Spec{
				Command:   "claude",
				Pkg:       "@anthropic-ai/claude-code",
				AuthPaths: []string{".credentials.json", "credentials.json"},
				Keychain:  keychainService,
				// Claude uses an API key from its environment instead of a login.
				AuthEnv: []string{"ANTHROPIC_API_KEY"},
				// The native installer puts it here, and that build updates itself.
				NativePaths:   []string{".local/bin/claude", ".claude/local/claude"},
				NativeUpdate:  []string{"claude", "update"},
				InstallScript: "curl -fsSL https://claude.ai/install.sh | bash",
				Account:       claudeAccount,
				// The subscription sign-in; with no terminal it prints the link and
				// reads the pasted code from stdin.
				LoginArgs:  []string{"auth", "login"},
				LoginRead:  claudeLoginLine,
				LoginStyle: clilogin.StylePaste,
				LogoutArgs: []string{"auth", "logout"},
			},
		},
	}
}

// claudeAccount reads oauthAccount from Claude Code's .claude.json.
func claudeAccount(path string) *clitool.Account {
	var file struct {
		OAuthAccount struct {
			Email string `json:"emailAddress"`
			Name  string `json:"displayName"`
			User  string `json:"accountUuid"`
			Org   string `json:"organizationUuid"`
		} `json:"oauthAccount"`
	}
	if !clitool.ReadJSON(path, &file) || file.OAuthAccount.Email == "" {
		return nil
	}
	a := file.OAuthAccount
	return &clitool.Account{Email: a.Email, Name: a.Name, Key: clitool.AccountKey(string(agent.KindClaude), a.User, a.Org)}
}

// Provider owns this host's runner and provider-specific capabilities.
type Provider struct {
	*runner
	sessions          *Sessions
	home, accountPath string
}

// New builds this host's Claude provider. A Root in deps (tests, the hermetic
// test host) puts the config directory and account file under it instead of
// the user's own.
func New(deps agent.Dependencies) *Provider {
	home := defaultHome()
	account := defaultConfigFile()
	if deps.Root != "" {
		home = filepath.Join(deps.Root, string(agent.KindClaude))
		account = filepath.Join(home, ".claude.json")
	}
	return &Provider{
		runner: &runner{deps: deps, executable: executable, home: home},
		home:   home, accountPath: account, sessions: NewSessions(home),
	}
}

// Name is what every screen calls it: "Claude".
func (p *Provider) Name() string { return definition().Tool.Name }

// Definition is the install configuration with this host's home and account
// paths, which the shared inventory installs, updates and signs in with.
func (p *Provider) Definition() agent.Provider {
	d := definition()
	d.Tool.Home = p.home
	d.Tool.AccountPath = p.accountPath
	return d
}

// Sessions reads this host's Claude transcripts.
func (p *Provider) Sessions() session.Provider { return p.sessions }

// Hooks installs and reports Claude's notification hooks in this host's home.
func (p *Provider) Hooks() notify.Provider { return Hooks{Home: p.home} }
