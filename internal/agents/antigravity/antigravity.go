// Package antigravity owns Google's Antigravity CLI (agy): its install
// definition, sign-in and models, and its chats, which run over the Agent
// Client Protocol (`agy acp`, the CLI's own server) on the host's ACP runner.
package antigravity

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agentcatalog"
	"github.com/repogoapp/host/internal/agents/acp"
	"github.com/repogoapp/host/internal/chatwire"
	"github.com/repogoapp/host/internal/clilogin"
	"github.com/repogoapp/host/internal/clitool"
	"github.com/repogoapp/host/internal/projectagent"
	"github.com/repogoapp/host/internal/session"
)

// Session option categories `agy acp` offers besides the model.
const (
	categoryMode   = "mode"
	categoryEffort = "thought_level"
)

// definition is Antigravity's descriptor and install configuration; status
// is the host's sign-in check, nil for the spec alone.
func definition(status *signIn) agent.Provider {
	if status == nil {
		status = &signIn{}
	}
	return agent.Provider{
		Company: "google",
		Tool: clitool.Tool{
			Kind:     string(agent.KindAntigravity),
			Name:     name,
			Category: clitool.CategoryAgent,
			Spec: clitool.Spec{
				Command: "agy",
				// Where Package links it; it updates by fetching again.
				NativePaths: []string{".local/bin/agy"},
				Package:     release,
				Latest:      latestVersion,
				// Its credentials are in the system keychain, so a status
				// command answers instead of a file.
				AuthProbe: status.probe,
				// A Google sign-in whose page shows a code to paste back.
				LoginArgs:   []string{"acp", "--login"},
				LoginRead:   loginLine,
				LoginStyle:  clilogin.StylePaste,
				AfterSignIn: func(context.Context, string) { status.clear() },
			},
		},
	}
}

// Provider is this host's Antigravity: the ACP runner configured for agy.
type Provider struct {
	deps   agent.Dependencies
	run    *acp.Provider
	status *signIn
	// home is agy's own folder, where it keeps its conversations; configDir
	// is where it reads hooks.json.
	home, configDir string
}

// New builds this host's Antigravity provider. A Root in deps (tests, the
// hermetic test host) keeps its chats under it.
func New(deps agent.Dependencies) *Provider {
	user, _ := os.UserHomeDir()
	home, configDir := filepath.Join(user, ".gemini", "antigravity-cli"), filepath.Join(user, ".gemini", "config")
	if deps.Root != "" {
		home = filepath.Join(deps.Root, string(agent.KindAntigravity), "agy")
		configDir = filepath.Join(deps.Root, string(agent.KindAntigravity), "config")
	}
	p := &Provider{deps: deps, status: &signIn{}, home: home, configDir: configDir}
	p.run = acp.NewWith(deps, acp.Config{
		Kind: agent.KindAntigravity, Name: name, Home: string(agent.KindAntigravity),
		Find:        p.manifest,
		Settings:    settings,
		Allows:      allows,
		OwnTools:    true,
		SessionMeta: systemPrompt,
		Steers:      true,
		MCPOverHTTP: true,
		Adopt:       p.adoptable,
		Label:       label,
	})
	return p
}

func (p *Provider) Kind() agent.Kind { return agent.KindAntigravity }
func (p *Provider) Name() string     { return name }

// name is what every screen calls it.
const name = "Antigravity"

// Definition is the install configuration with this host's agy folder, which
// the shared inventory installs, updates and signs in with.
func (p *Provider) Definition() agent.Provider {
	d := definition(p.status)
	d.Tool.Home = p.home
	return d
}

// Available is false until agy is installed; a signed-out agy says so in
// its turn.
func (p *Provider) Available() error {
	if err := p.run.Available(); err != nil {
		return err
	}
	if _, err := exec.LookPath(executable()); err != nil {
		return errors.New("Antigravity CLI not installed")
	}
	return nil
}

func (p *Provider) Send(ctx context.Context, req agent.TurnRequest, io agent.TurnIO) (agent.Result, error) {
	return p.run.Send(ctx, req, io)
}

// Steer sends req into the chat's running turn; agy reads it before its next
// step.
func (p *Provider) Steer(ctx context.Context, turnID string, req agent.TurnRequest) error {
	return p.run.Steer(ctx, turnID, req)
}

// Sessions is Antigravity's chats: RepoGo's, and the ones agy ran on its own.
func (p *Provider) Sessions() session.Provider { return &history{host: p.run.Chats(), p: p} }

// adoptable is the folder of a conversation agy ran on its own, for a turn
// that continues it from RepoGo.
func (p *Provider) adoptable(id string) (string, bool) {
	if _, err := os.Stat(filepath.Join(p.home, "brain", id, ".system_generated", "logs", usageLog)); err != nil {
		return "", false
	}
	cwd := p.conversations()[id].cwd
	return cwd, cwd != ""
}

func (p *Provider) ToolRow(call agent.ToolCall) chatwire.Tool             { return p.run.ToolRow(call) }
func (p *Provider) Resolve(call agent.ToolCall) agent.ToolCall            { return p.run.Resolve(call) }
func (p *Provider) Close()                                                { p.run.Close() }
func (p *Provider) manifest(cwd, _ string) (projectagent.Manifest, error) { return manifest(cwd), nil }

// manifest runs `agy acp` in the chat's folder, one process per folder: a
// session's tools work where the process runs.
func manifest(cwd string) projectagent.Manifest {
	bin := executable()
	return projectagent.Manifest{
		ID: string(agent.KindAntigravity), Name: name,
		Command: bin, Args: []string{"acp"},
		Dir: bin, Project: cwd, Fingerprint: bin,
	}
}

// executable is the native install when there is one, else agy on PATH.
func executable() string { return clitool.PreferredExecutable(definition(nil).Tool.Spec) }

// Catalog is the models agy's sessions offer and the thinking levels of each,
// read from a session opened for it in a folder of its own.
func (p *Provider) Catalog(ctx context.Context) (agentcatalog.Catalog, error) {
	if _, err := exec.LookPath(executable()); err != nil {
		return agentcatalog.Catalog{}, errors.New("agy not installed")
	}
	dir := filepath.Join(os.TempDir(), "repogo-antigravity")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return agentcatalog.Catalog{}, err
	}
	choices, err := p.run.Models(ctx, dir, categoryEffort)
	if err != nil {
		return agentcatalog.Catalog{}, err
	}
	out := agentcatalog.Catalog{Available: true, Models: make([]agentcatalog.Model, 0, len(choices))}
	for _, c := range choices {
		m := agentcatalog.Model{
			ID: c.ID, Name: c.Name, Default: c.Default, Company: company(c.ID),
			Efforts: c.Efforts, DefaultEffort: c.DefaultEffort,
		}
		if m.Efforts == nil {
			m.Efforts = []string{}
		}
		if c.Default {
			out.DefaultModel = c.ID
		}
		out.Models = append(out.Models, m)
	}
	return out, nil
}

// company is who makes a model agy runs, when it isn't Google: the phone asks
// consent per company.
func company(id string) string {
	switch {
	case strings.HasPrefix(id, "claude"):
		return "anthropic"
	case strings.HasPrefix(id, "gpt"):
		return "openai"
	}
	return ""
}

// settings is RepoGo's turn config as agy's session options: plan mode, else
// the permission mode as agy's own, and the thinking level, falling back to
// the next level down where a model has no Extra (xhigh).
func settings(cfg agent.TurnConfig) map[string][]string {
	mode := "default"
	switch {
	case cfg.Mode == "plan":
		mode = "plan"
	case cfg.PermissionMode == agent.PermissionAutoAcceptEdits:
		mode = "accept-edits"
	case cfg.PermissionMode == agent.PermissionFullAccess:
		mode = "auto"
	}
	out := map[string][]string{categoryMode: {mode}}
	switch cfg.ReasoningLevel {
	case "low", "medium", "high":
		out[categoryEffort] = []string{cfg.ReasoningLevel}
	case "xhigh", "max":
		out[categoryEffort] = []string{"xhigh", "high"}
	}
	return out
}

// allows is what a turn runs without asking: RepoGo's own tools, which agy
// names mcp_<server>_<tool>, and every tool in full access, where agy's
// auto mode still asks before what it can't undo.
func allows(cfg agent.TurnConfig, tool string) bool {
	return cfg.PermissionMode == agent.PermissionFullAccess || strings.HasPrefix(tool, "mcp_"+agent.ToolsServer+"_")
}

// systemPrompt is RepoGo's instructions as agy takes them on session/new.
func systemPrompt(text string) any {
	return map[string]any{"antigravity": map[string]any{"systemPrompt": map[string]string{"append": text}}}
}
