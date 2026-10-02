package claude

import (
	"encoding/json"
	"testing"
)

// Every model that takes effort reports the one in force: a per-model
// setting, else the global one, else what the CLI applies with none set.
func TestCatalogReportsDefaultEffort(t *testing.T) {
	models := json.RawMessage(`{"models":[
		{"value":"opus","resolvedModel":"claude-opus-5-5","displayName":"Opus 5.5","supportsEffort":true,"supportedEffortLevels":["low","medium","high"]},
		{"value":"sonnet","resolvedModel":"claude-sonnet-5","displayName":"Sonnet 5","supportsEffort":true,"supportedEffortLevels":["low","medium","high"]},
		{"value":"haiku","resolvedModel":"claude-haiku-4-5","displayName":"Haiku 4.5"}]}`)

	for _, tc := range []struct {
		name, settings, wantAgent, wantOpus, wantSonnet string
	}{
		{"applied", `{"effective":{},"applied":{"effort":"high"}}`, "high", "high", "high"},
		{"global", `{"effective":{"effortLevel":"low"},"applied":{"effort":"high"}}`, "low", "low", "low"},
		{"per model", `{"effective":{"effortLevel":"low","modelSettings":{"claude-sonnet-5":{"effortLevel":"medium"}}}}`, "low", "low", "medium"},
	} {
		got := catalogFrom(map[string]json.RawMessage{"models": models, "settings": json.RawMessage(tc.settings)})
		if got.DefaultEffort != tc.wantAgent {
			t.Errorf("%s: agent default effort = %q, want %q", tc.name, got.DefaultEffort, tc.wantAgent)
		}
		if e := got.Models[0].DefaultEffort; e != tc.wantOpus {
			t.Errorf("%s: opus default effort = %q, want %q", tc.name, e, tc.wantOpus)
		}
		if e := got.Models[1].DefaultEffort; e != tc.wantSonnet {
			t.Errorf("%s: sonnet default effort = %q, want %q", tc.name, e, tc.wantSonnet)
		}
		if e := got.Models[2].DefaultEffort; e != "" {
			t.Errorf("%s: haiku takes no effort, default = %q", tc.name, e)
		}
	}
}

func TestWithoutContext(t *testing.T) {
	for in, want := range map[string]string{
		"Opus (1M context)": "Opus",
		"Opus 5.5 with 1M context · Best for everyday, complex tasks": "Opus 5.5 · Best for everyday, complex tasks",
		"Sonnet (200k context)":                 "Sonnet",
		"Fable":                                 "Fable",
		"Haiku 4.5 · Fastest for quick answers": "Haiku 4.5 · Fastest for quick answers",
	} {
		if got := withoutContext(in); got != want {
			t.Errorf("withoutContext(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every id the host has recorded off Claude transcripts, named as a phone shows it.
func TestModelName(t *testing.T) {
	var p Provider
	for id, want := range map[string]string{
		"claude-opus-5-5":            "Opus 5.5",
		"claude-opus-5":              "Opus 5",
		"claude-fable-5-1":           "Fable 5.1",
		"claude-haiku-4-5-20251001":  "Haiku 4.5",
		"claude-opus-5-5[1m]":        "Opus 5.5",
		"claude-3-5-sonnet-20241022": "Sonnet 3.5",
		"opus":                       "",
		"claude":                     "",
		"gpt-5.5":                    "",
		"":                           "",
	} {
		if got := p.ModelName(id); got != want {
			t.Errorf("ModelName(%q) = %q, want %q", id, got, want)
		}
	}
}
