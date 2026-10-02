// Package cursor adapts the official Cursor CLI to RepoGo's host contracts.
package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/clilogin"
	"github.com/repogo/host/internal/clitool"
	"github.com/repogo/host/internal/session"
)

type Provider struct {
	deps       agent.Dependencies
	home       string
	executable func() string
	pool       agent.SessionPool[*liveSession]
	sessions   *Sessions
}

func New(deps agent.Dependencies) *Provider {
	home := agent.ConfigHome("CURSOR_CONFIG_DIR", ".cursor")
	if os.Getenv("CURSOR_CONFIG_DIR") == "" && os.Getenv("XDG_CONFIG_HOME") != "" {
		home = filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "cursor")
	}
	if deps.Root != "" {
		home = filepath.Join(deps.Root, "cursor")
	}
	p := &Provider{deps: deps, home: home, executable: func() string { return clitool.PreferredExecutable(installSpec()) }}
	p.sessions = &Sessions{provider: p, dir: filepath.Join(home, "acp-sessions"), cache: map[string]*snapshotCache{}}
	return p
}
func (*Provider) Kind() agent.Kind             { return agent.KindCursor }
func (*Provider) Name() string                 { return "Cursor" }
func (p *Provider) Sessions() session.Provider { return p.sessions }
func (p *Provider) Close()                     { p.pool.Close() }
func (p *Provider) Available() error {
	if p.deps.Context == nil || p.deps.Log == nil {
		return errors.New("Cursor requires host context and logger")
	}
	if _, err := exec.LookPath(p.executable()); err != nil {
		return errors.New("Cursor CLI not installed")
	}
	return nil
}
func installSpec() clitool.Spec {
	return clitool.Spec{
		Command: "cursor-agent", NativePaths: []string{".local/bin/cursor-agent", ".local/bin/agent"},
		NativeUpdate: []string{"cursor-agent", "update"}, InstallScript: "curl -fsSL https://cursor.com/install | bash",
		LoginArgs: []string{"login"}, LoginEnv: []string{"NO_OPEN_BROWSER=1"}, LoginRead: loginLine,
		LogoutArgs: []string{"logout"},
	}
}
func (p *Provider) Definition() agent.Provider {
	spec := installSpec()
	spec.AuthProbe = p.authenticated
	spec.LoginEnv = append(p.environment(), "NO_OPEN_BROWSER=1")
	return agent.Provider{Company: "cursor", Tool: clitool.Tool{
		Kind: string(p.Kind()), Name: p.Name(), Icon: "agent-cursor", Category: clitool.CategoryAgent, Home: p.home, Spec: spec,
	}}
}

var loginURL = regexp.MustCompile(`https://(?:www\.)?cursor\.com/[^\s\x1b]+`)

func loginLine(line string, code *clilogin.Code) bool {
	if u := loginURL.FindString(line); u != "" {
		code.URL = strings.TrimRight(u, ".,)")
	}
	return code.URL != ""
}

func (p *Provider) environment() []string {
	env := agent.ChildEnv()
	if p.deps.Root != "" {
		clean := env[:0]
		for _, item := range env {
			key, _, _ := strings.Cut(item, "=")
			if key != "CURSOR_API_KEY" && key != "CURSOR_AUTH_TOKEN" {
				clean = append(clean, item)
			}
		}
		env = append(clean, "HOME="+p.deps.Root, "CURSOR_DATA_DIR="+p.home)
	}
	env = append(env, p.deps.Env...)
	return append(env, "CURSOR_CONFIG_DIR="+p.home)
}

// Auth status comes from the CLI so the provider never decodes credential storage.
func (p *Provider) authenticated(ctx context.Context, path string) (bool, string, *clitool.Account) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "status", "--format", "json")
	cmd.Env = p.environment()
	cmd.Dir = os.TempDir()
	out, err := cmd.Output()
	if err != nil {
		return false, "", nil
	}
	var status struct {
		IsAuthenticated bool `json:"isAuthenticated"`
		UserInfo        struct {
			Email     string `json:"email"`
			FirstName string `json:"firstName"`
			LastName  string `json:"lastName"`
		} `json:"userInfo"`
	}
	if json.Unmarshal(out, &status) != nil || !status.IsAuthenticated {
		return false, "", nil
	}
	var account *clitool.Account
	if status.UserInfo.Email != "" {
		account = &clitool.Account{Email: status.UserInfo.Email, Name: strings.TrimSpace(status.UserInfo.FirstName + " " + status.UserInfo.LastName)}
	}
	return true, "cli", account
}
