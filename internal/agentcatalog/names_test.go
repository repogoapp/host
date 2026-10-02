package agentcatalog

import (
	"context"
	"strings"
	"testing"

	"github.com/repogo/host/internal/agent"
)

// namingProvider names `model-…` ids, as an adapter names its own id shape,
// to show that a catalog entry's name wins and an unlisted id is read off itself.
type namingProvider struct{ catalog Catalog }

func (namingProvider) Kind() agent.Kind                  { return agent.KindClaude }
func (p namingProvider) Catalog(context.Context) Catalog { return p.catalog }
func (namingProvider) ModelName(id string) string {
	if rest, ok := strings.CutPrefix(id, "model-"); ok {
		return "Model " + rest
	}
	return ""
}

var claudeCatalog = Catalog{Available: true, Models: []Model{
	{ID: "default", Resolved: "model-5", Name: "Default (recommended)", Hidden: true},
	{ID: "opus", Resolved: "model-5", Name: "Opus"},
	{ID: "opus[1m]", Resolved: "model-5[1m]", Name: "Opus (1M context)"},
	{ID: "sonnet", Resolved: "model-4", Name: "Sonnet"},
}}

func TestCatalogIDJoinsATranscriptToItsAlias(t *testing.T) {
	listed := claudeCatalog.Models[1:]
	for transcript, want := range map[string]string{
		"sonnet":      "sonnet",
		"model-4":     "sonnet",
		"model-5":     "opus",
		"model-5[1m]": "opus[1m]",
		"model-9":     "",
		"":            "",
	} {
		if got := CatalogID(transcript, listed); got != want {
			t.Errorf("CatalogID(%q) = %q, want %q", transcript, got, want)
		}
	}

	// The alias alone carries the context window.
	aliasOnly := []Model{{ID: "opus", Resolved: "model-5"}, {ID: "opus[1m]", Resolved: "model-5"}}
	if got := CatalogID("model-5[1m]", aliasOnly); got != "opus[1m]" {
		t.Errorf("CatalogID(model-5[1m]) = %q, want opus[1m]", got)
	}
}

func TestModelLabel(t *testing.T) {
	s := New([]agent.Kind{agent.KindClaude}, namingProvider{claudeCatalog})

	// Before the catalog is read, an id is named off itself.
	if got := s.ModelLabel(agent.KindClaude, "model-5"); got != "Model 5" {
		t.Errorf("before the catalog: %q, want Model 5", got)
	}

	s.One(t.Context(), agent.KindClaude, false)
	for id, want := range map[string]string{
		"model-5":     "Opus",
		"model-5[1m]": "Opus (1M context)",
		"default":     "Default (recommended)",
		"model-3":     "Model 3",
		"o4-mini":     "o4-mini",
		"":            "",
	} {
		if got := s.ModelLabel(agent.KindClaude, id); got != want {
			t.Errorf("ModelLabel(%q) = %q, want %q", id, got, want)
		}
	}
}
