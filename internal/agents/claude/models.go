package claude

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode"

	"github.com/repogoapp/host/internal/agentcatalog"
)

// Catalog drives the CLI's stream-json control channel: `list_models` is the
// same list the in-app picker shows, with per-model effort and fast-mode
// support, and `get_settings` is what is in force when nothing is chosen.
func (p *Provider) Catalog(ctx context.Context) (agentcatalog.Catalog, error) {
	res, ok := streamJSONControl(ctx, map[string]string{
		"models":   "list_models",
		"settings": "get_settings",
	})
	if !ok {
		return agentcatalog.Catalog{}, errors.New("claude CLI unavailable or did not answer list_models")
	}
	return catalogFrom(res)
}

// catalogFrom builds the catalog from the CLI's answers, keyed as Catalog
// asks for them.
func catalogFrom(res map[string]json.RawMessage) (agentcatalog.Catalog, error) {
	var models struct {
		Models []struct {
			Value            string   `json:"value"`
			Resolved         string   `json:"resolvedModel"`
			DisplayName      string   `json:"displayName"`
			SupportsEffort   bool     `json:"supportsEffort"`
			Efforts          []string `json:"supportedEffortLevels"`
			AdaptiveThinking bool     `json:"supportsAdaptiveThinking"`
			FastMode         bool     `json:"supportsFastMode"`
		} `json:"models"`
	}
	if json.Unmarshal(res["models"], &models) != nil || len(models.Models) == 0 {
		return agentcatalog.Catalog{}, errors.New("claude list_models answered with no models")
	}

	var settings struct {
		Effective struct {
			Model         string `json:"model"`
			EffortLevel   string `json:"effortLevel"`
			ModelSettings map[string]struct {
				EffortLevel string `json:"effortLevel"`
			} `json:"modelSettings"`
		} `json:"effective"`
		// What the CLI runs with, set or not: its own effort when settings name none.
		Applied struct {
			Effort string `json:"effort"`
		} `json:"applied"`
	}
	_ = json.Unmarshal(res["settings"], &settings)
	eff := settings.Effective
	if eff.EffortLevel == "" {
		eff.EffortLevel = settings.Applied.Effort
	}

	catalog := agentcatalog.Catalog{Available: true, DefaultModel: eff.Model}

	// The CLI's literal `default` entry only says what it resolves to, and the
	// app draws its own default row: hide it and mark the real model, the
	// effective one from settings, else whichever resolves the same way.
	defaultModel := eff.Model
	if defaultModel == "" || defaultModel == "default" {
		for _, m := range models.Models {
			if m.Value == "default" {
				defaultModel = m.Resolved
				break
			}
		}
	}
	for _, m := range models.Models {
		isDefault := m.Value == defaultModel || (m.Value != "default" && m.Resolved == defaultModel)
		model := agentcatalog.Model{
			ID: m.Value, Name: withoutContext(m.DisplayName),
			Resolved: m.Resolved, Default: isDefault, Hidden: m.Value == "default",
			Efforts:          []string{},
			FastMode:         m.FastMode,
			AdaptiveThinking: m.AdaptiveThinking,
		}
		if m.SupportsEffort {
			model.Efforts = append(model.Efforts, m.Efforts...)
			// A per-model override wins over the global level, keyed by the
			// resolved name without its context suffix.
			model.DefaultEffort = eff.EffortLevel
			if ms, ok := eff.ModelSettings[m.Resolved]; ok && ms.EffortLevel != "" {
				model.DefaultEffort = ms.EffortLevel
			}
		}
		catalog.Models = append(catalog.Models, model)
	}
	return catalog, nil
}

// contextNote is the window size Claude writes into labels: "Opus (1M
// context)", "Opus 5.5 with 1M context · …".
var contextNote = regexp.MustCompile(`(?i)\s*(\([^)]*\bcontext\)|\bwith [\d.]+[km] context\b)`)

// withoutContext drops the window size from a label, so Claude's list reads
// like Codex's: one name per model. The id keeps its `[1m]`, because that is
// what the CLI runs.
func withoutContext(label string) string {
	return strings.TrimSpace(contextNote.ReplaceAllString(label, ""))
}

// ModelName reads a Claude model id as a name, family then version:
// `claude-opus-5-5` → "Opus 5.5", `claude-haiku-4-5-20251001` → "Haiku 4.5",
// `claude-3-5-sonnet-20241022` → "Sonnet 3.5". Empty for any other id.
func (p *Provider) ModelName(id string) string {
	parts := idParts(id)
	if len(parts) < 2 || !strings.EqualFold(parts[0], "claude") {
		return ""
	}
	var words, version []string
	for _, part := range parts[1:] {
		switch {
		case strings.IndexFunc(part, func(r rune) bool { return !unicode.IsLetter(r) }) < 0:
			words = append(words, part)
		case len(part) <= 2 && strings.IndexFunc(part, func(r rune) bool { return !unicode.IsDigit(r) }) < 0:
			version = append(version, part)
		}
	}
	if len(words) == 0 {
		return ""
	}
	if len(version) == 0 {
		return agentcatalog.WordName(words)
	}
	return agentcatalog.WordName(words) + " " + strings.Join(version, ".")
}

// idParts is a model id's dash-separated parts without its context window:
// `claude-opus-5-5[1m]` → claude, opus, 5, 5.
func idParts(id string) []string {
	base, _, _ := strings.Cut(id, "[")
	return strings.FieldsFunc(base, func(r rune) bool { return r == '-' })
}
