package acp

import (
	"context"
	"errors"

	"github.com/repogoapp/host/internal/projectagent"
)

// ModelChoice is one model a shipped agent's sessions offer: its id as the
// model option takes it, and the values its effort option takes on it.
type ModelChoice struct {
	ID, Name      string
	Efforts       []string
	DefaultEffort string
	Default       bool
}

// Models asks the agent what a session in cwd offers, in a process of its
// own that ends with the answer: each model of its model option and, by
// setting each in turn, the effort option's values on it (effort is that
// option's category). Opening a session records
// no conversation until it is prompted.
func (p *Provider) Models(ctx context.Context, cwd, effort string) ([]ModelChoice, error) {
	m, err := p.cfg.Find(cwd, "")
	if err != nil {
		return nil, err
	}
	proc, err := p.start(ctx, m, "")
	if err != nil {
		return nil, err
	}
	defer proc.close()
	var opened newSessionResult
	if err := proc.client.Call(ctx, methodNewSession, sessionParams{Cwd: proc.workdir, MCPServers: []mcpServer{}}, &opened); err != nil {
		return nil, failure{name: proc.name, step: "couldn't open a chat", err: err}
	}
	models, ok := modelOption(opened.ConfigOptions)
	if !ok {
		return nil, errors.New(proc.name + " offers no model option")
	}
	options := opened.ConfigOptions
	out := make([]ModelChoice, 0, len(models.Models))
	for _, model := range models.Models {
		if model.ID != models.Current {
			var set sessionResult
			err := proc.client.Call(ctx, methodSetConfig, setConfigParams{SessionID: opened.SessionID, ConfigID: models.ConfigID, Value: model.ID}, &set)
			if err != nil {
				return nil, failure{name: proc.name, step: "couldn't switch to " + model.ID, err: err}
			}
			options = set.ConfigOptions
		}
		choice := ModelChoice{ID: model.ID, Name: model.Name, Default: model.ID == models.Current}
		if o, ok := optionIn(options, effort); ok && o.Category == effort {
			choice.DefaultEffort = o.CurrentValue
			for _, c := range choices(o.Options) {
				choice.Efforts = append(choice.Efforts, c.ID)
			}
		}
		out = append(out, choice)
	}
	return out, nil
}

// Manifest is the agent a chat in cwd runs, for a caller that names it.
func (p *Provider) Manifest(cwd string) (projectagent.Manifest, error) { return p.cfg.Find(cwd, "") }
