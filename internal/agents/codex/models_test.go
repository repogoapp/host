package codex

import (
	"encoding/json"
	"testing"
)

// Fast mode is offered on a model whose speed tiers include "fast", reported
// as the "fast" tier the app toggles, though its service tier is "priority".
func TestCatalogReportsFastSpeedTier(t *testing.T) {
	res := map[string]json.RawMessage{
		"models": json.RawMessage(`{"data":[
			{"model":"gpt-6-astra","displayName":"GPT-6 Astra","additionalSpeedTiers":["fast"],
			 "serviceTiers":[{"id":"priority","name":"Fast","description":"2x speed, increased usage"}]},
			{"model":"gpt-5.5","displayName":"GPT-5.5","additionalSpeedTiers":[],"serviceTiers":[]}]}`),
		"config": json.RawMessage(`{"config":{"model":"gpt-6-astra","service_tier":"priority"}}`),
	}

	got := catalogFrom(res)
	if !got.Available || !got.FastMode || !got.FastModeDefault {
		t.Fatalf("catalog = available %v, fast %v, fast default %v; want all true", got.Available, got.FastMode, got.FastModeDefault)
	}
	if tiers := got.Models[0].SpeedTiers; len(tiers) != 1 || tiers[0].Value != "fast" {
		t.Fatalf("astra speed tiers = %+v, want one \"fast\"", tiers)
	}
	if tiers := got.Models[1].SpeedTiers; len(tiers) != 0 {
		t.Fatalf("gpt-5.5 speed tiers = %+v, want none", tiers)
	}
}

func TestFastTier(t *testing.T) {
	for tier, want := range map[string]bool{"fast": true, "priority": true, "default": false, "flex": false, "": false} {
		if got := fastTier(tier); got != want {
			t.Errorf("fastTier(%q) = %v, want %v", tier, got, want)
		}
	}
}

func TestModelName(t *testing.T) {
	var p Provider
	for id, want := range map[string]string{
		"gpt-5.6-sol":       "Sol 5.6",
		"gpt-6-astra":       "Astra 6",
		"gpt-5-codex":       "Codex 5",
		"gpt-5.1-codex-max": "Codex Max 5.1",
		"gpt-5.5":           "GPT 5.5",
		"o4-mini":           "",
		"":                  "",
	} {
		if got := p.ModelName(id); got != want {
			t.Errorf("ModelName(%q) = %q, want %q", id, got, want)
		}
	}
}
