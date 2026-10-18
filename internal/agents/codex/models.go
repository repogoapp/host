package codex

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/repogoapp/host/internal/agentcatalog"
)

// Catalog drives the app-server: `model/list` for models with their efforts
// and speed tiers, and `config/read` for what is in force.
func (p *Provider) Catalog(ctx context.Context) (agentcatalog.Catalog, error) {
	res, ok := appServer(ctx, map[string]string{
		"models": "model/list",
		"config": "config/read",
	})
	if !ok {
		return agentcatalog.Catalog{}, errors.New("codex app-server unavailable or did not answer model/list")
	}
	return catalogFrom(res)
}

// catalogFrom builds the catalog from the app-server's answers, keyed as
// Catalog asks for them.
func catalogFrom(res map[string]json.RawMessage) (agentcatalog.Catalog, error) {
	var models struct {
		Data []struct {
			ID          string `json:"id"`
			Model       string `json:"model"`
			DisplayName string `json:"displayName"`
			Hidden      bool   `json:"hidden"`
			IsDefault   bool   `json:"isDefault"`
			Efforts     []struct {
				Effort string `json:"reasoningEffort"`
			} `json:"supportedReasoningEfforts"`
			DefaultEffort string   `json:"defaultReasoningEffort"`
			SpeedTiers    []string `json:"additionalSpeedTiers"`
		} `json:"data"`
	}
	if json.Unmarshal(res["models"], &models) != nil || len(models.Data) == 0 {
		return agentcatalog.Catalog{}, errors.New("codex model/list answered with no models")
	}

	var config struct {
		Config struct {
			Model  string `json:"model"`
			Effort string `json:"model_reasoning_effort"`
		} `json:"config"`
	}
	_ = json.Unmarshal(res["config"], &config)
	cfg := config.Config

	catalog := agentcatalog.Catalog{Available: true, DefaultModel: cfg.Model}
	for _, m := range models.Data {
		id := m.Model
		if id == "" {
			id = m.ID
		}
		isDefault := m.IsDefault
		if cfg.Model != "" {
			isDefault = id == cfg.Model
		}
		model := agentcatalog.Model{
			ID: id, Name: m.DisplayName, Default: isDefault, Hidden: m.Hidden,
			Efforts: []string{}, DefaultEffort: m.DefaultEffort,
			// Fast mode is offered only on a model whose speed tiers include
			// "fast"; the service tiers name it "priority".
			FastMode: slices.Contains(m.SpeedTiers, "fast"),
		}
		for _, e := range m.Efforts {
			model.Efforts = append(model.Efforts, e.Effort)
		}
		if isDefault && cfg.Effort != "" {
			model.DefaultEffort = cfg.Effort
		}
		catalog.Models = append(catalog.Models, model)
	}
	return catalog, nil
}

// fastTier reports whether a service tier is fast mode, which Codex has
// written as both "fast" and "priority".
func fastTier(tier string) bool {
	return tier == "fast" || tier == "priority"
}

// ModelName reads a GPT model id the way Claude's read, variant then version:
// `gpt-5.6-sol` → "Sol 5.6", `gpt-5-codex` → "Codex 5", `gpt-5.5` → "GPT 5.5".
// Empty for any other id.
func (p *Provider) ModelName(id string) string {
	base, _, _ := strings.Cut(id, "[")
	parts := strings.FieldsFunc(base, func(r rune) bool { return r == '-' })
	if len(parts) < 2 || !strings.EqualFold(parts[0], "gpt") {
		return ""
	}
	version, variant := parts[1], agentcatalog.WordName(parts[2:])
	if variant == "" {
		return "GPT " + version
	}
	return variant + " " + version
}
