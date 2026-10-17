package acp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/repogoapp/host/internal/projectagent"
)

// agentModels is the model option an agent reported for its sessions: the
// option's id, to set it, its value, and the choices.
type agentModels struct {
	ConfigID string                           `json:"config_id"`
	Current  string                           `json:"current"`
	Models   []projectagent.ProjectAgentModel `json:"models"`
}

// modelOption finds the option that picks the model among a session's
// options: the one in the model category, else one named "model".
func modelOption(options []configOption) (agentModels, bool) {
	for _, o := range options {
		if o.Category == categoryModel {
			if m, ok := selectModels(o); ok {
				return m, true
			}
		}
	}
	for _, o := range options {
		if o.ID == categoryModel {
			if m, ok := selectModels(o); ok {
				return m, true
			}
		}
	}
	return agentModels{}, false
}

// selectModels is a select option read as models; false for any other kind.
func selectModels(o configOption) (agentModels, bool) {
	models := choices(o.Options)
	if o.Type != "select" || len(models) == 0 {
		return agentModels{}, false
	}
	return agentModels{ConfigID: o.ID, Current: o.CurrentValue, Models: models}, true
}

// choices flattens a select's options, grouped or not, into models.
func choices(raw json.RawMessage) []projectagent.ProjectAgentModel {
	var list []configChoice
	if json.Unmarshal(raw, &list) != nil {
		return nil
	}
	var out []projectagent.ProjectAgentModel
	for _, c := range list {
		if c.Value != "" {
			out = append(out, projectagent.ProjectAgentModel{ID: c.Value, Name: c.Name})
		}
		for _, inner := range c.Options {
			if inner.Value != "" {
				out = append(out, projectagent.ProjectAgentModel{ID: inner.Value, Name: inner.Name})
			}
		}
	}
	return out
}

// reported keeps the models each agent reported, by manifest fingerprint,
// so the picker lists them before a chat starts its process.
type reported struct {
	mu   sync.Mutex
	path string
}

func (r *reported) get(fingerprint string) (agentModels, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.read()[fingerprint]
	return m, ok
}

func (r *reported) put(fingerprint string, m agentModels) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	all := r.read()
	all[fingerprint] = m
	raw, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(r.path, raw, 0o600)
}

func (r *reported) read() map[string]agentModels {
	all := map[string]agentModels{}
	raw, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return all
	}
	_ = json.Unmarshal(raw, &all)
	return all
}
