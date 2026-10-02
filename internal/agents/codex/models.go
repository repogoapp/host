package codex

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/repogo/host/internal/agentcatalog"
)

// Codex's policies are enums in its protocol schema, not a list the server
// answers; sandboxes are the exception and are read live.
var codexApprovalPolicies = []agentcatalog.Choice{
	{Value: "untrusted", Name: "Untrusted", Description: "Ask before anything but known-safe reads"},
	{Value: "on-request", Name: "On request", Description: "The model asks when it wants to"},
	{Value: "never", Name: "Never", Description: "Never ask; blocked actions fail"},
}

// codexModes is the picker's mode choice: a normal turn, or plan mode.
var codexModes = []agentcatalog.Choice{
	{Value: "default", Name: "Agent", Description: "Read, edit and run"},
	{Value: "plan", Name: "Plan", Description: "Read and propose; make no changes"},
}

// codexReviewers are who answers an approval prompt.
var codexReviewers = []agentcatalog.Choice{
	{Value: "user", Name: "You", Description: "Approval prompts reach the user"},
	{Value: "auto_review", Name: "Auto review", Description: "A subagent weighs the risk and answers"},
}

// codexPersonalities are the tones Codex's personality setting accepts.
var codexPersonalities = []agentcatalog.Choice{
	{Value: "none", Name: "None"},
	{Value: "friendly", Name: "Friendly"},
	{Value: "pragmatic", Name: "Pragmatic"},
}

// Sandboxes have profile ids (`:read-only`) and config values (`read-only`);
// the value is what a thread takes, so that is the one reported.
var codexSandboxNames = map[string]agentcatalog.Choice{
	"read-only":          {Value: "read-only", Name: "Read only", Description: "No writes, no network"},
	"workspace-write":    {Value: "workspace-write", Name: "Workspace", Description: "Write inside the project"},
	"danger-full-access": {Value: "danger-full-access", Name: "Full access", Description: "No sandbox"},
}

// Catalog drives the app-server: `model/list` for models with their efforts
// and speed tiers, `permissionProfile/list` for which sandboxes this install
// allows, and `config/read` for what is in force.
func (p *Provider) Catalog(ctx context.Context) agentcatalog.Catalog {
	res, ok := appServer(ctx, map[string]string{
		"models":   "model/list",
		"profiles": "permissionProfile/list",
		"config":   "config/read",
	})
	if !ok {
		return agentcatalog.Unavailable("codex app-server unavailable or did not answer model/list")
	}
	return catalogFrom(res)
}

// catalogFrom builds the catalog from the app-server's answers, keyed as
// Catalog asks for them.
func catalogFrom(res map[string]json.RawMessage) agentcatalog.Catalog {
	var models struct {
		Data []struct {
			ID          string `json:"id"`
			Model       string `json:"model"`
			DisplayName string `json:"displayName"`
			Description string `json:"description"`
			Hidden      bool   `json:"hidden"`
			IsDefault   bool   `json:"isDefault"`
			Efforts     []struct {
				Effort      string `json:"reasoningEffort"`
				Description string `json:"description"`
			} `json:"supportedReasoningEfforts"`
			DefaultEffort string   `json:"defaultReasoningEffort"`
			Modalities    []string `json:"inputModalities"`
			SpeedTiers    []string `json:"additionalSpeedTiers"`
		} `json:"data"`
	}
	if json.Unmarshal(res["models"], &models) != nil || len(models.Data) == 0 {
		return agentcatalog.Unavailable("codex model/list answered with no models")
	}

	var config struct {
		Config struct {
			Model       string `json:"model"`
			Effort      string `json:"model_reasoning_effort"`
			Approval    string `json:"approval_policy"`
			Sandbox     string `json:"sandbox_mode"`
			ServiceTier string `json:"service_tier"`
			Personality string `json:"personality"`
		} `json:"config"`
	}
	_ = json.Unmarshal(res["config"], &config)
	cfg := config.Config

	catalog := agentcatalog.Catalog{
		Available:          true,
		Detail:             "live from codex app-server model/list",
		CapturedAtMS:       time.Now().UnixMilli(),
		Modes:              codexModes,
		PermissionModes:    codexApprovalPolicies,
		DefaultPermission:  cfg.Approval,
		Reviewers:          codexReviewers,
		Personalities:      codexPersonalities,
		DefaultPersonality: cfg.Personality,
		DefaultModel:       cfg.Model,
		DefaultEffort:      cfg.Effort,
		DefaultSandbox:     cfg.Sandbox,
		FastModeDefault:    fastTier(cfg.ServiceTier),
	}

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
			ID: id, Name: m.DisplayName, Description: m.Description,
			Default: isDefault, Hidden: m.Hidden,
			Efforts: []agentcatalog.Choice{}, DefaultEffort: m.DefaultEffort,
			Modalities: m.Modalities,
		}
		for _, e := range m.Efforts {
			model.Efforts = append(model.Efforts, agentcatalog.Choice{Value: e.Effort, Name: e.Effort, Description: e.Description})
		}
		// Fast mode is offered only on a model whose speed tiers include
		// "fast"; the service tiers name it "priority".
		if slices.Contains(m.SpeedTiers, "fast") {
			model.SpeedTiers = []agentcatalog.Choice{{Value: "fast", Name: "Fast", Description: "Faster output; uses more quota"}}
			catalog.FastMode = true
		}
		if isDefault && cfg.Effort != "" {
			model.DefaultEffort = cfg.Effort
		}
		catalog.Models = append(catalog.Models, model)
	}

	catalog.Sandboxes = sandboxes(res["profiles"])
	return catalog
}

// sandboxes is the permission profiles this install allows, as sandbox choices.
func sandboxes(raw json.RawMessage) []agentcatalog.Choice {
	var profiles struct {
		Data []struct {
			ID      string `json:"id"`
			Allowed bool   `json:"allowed"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &profiles)
	var out []agentcatalog.Choice
	for _, profile := range profiles.Data {
		if !profile.Allowed {
			continue
		}
		value := strings.TrimPrefix(profile.ID, ":")
		if value == "workspace" {
			value = "workspace-write"
		}
		choice, known := codexSandboxNames[value]
		if !known {
			choice = agentcatalog.Choice{Value: value, Name: value}
		}
		out = append(out, choice)
	}
	return out
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
