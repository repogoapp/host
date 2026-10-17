package acp

import (
	"encoding/json"
	"testing"

	"github.com/repogoapp/host/internal/agent"
)

func TestToolRowIsTheAgentsTitleWithAnIconForItsInput(t *testing.T) {
	p := &Provider{}
	for _, c := range []struct {
		title, input, icon string
	}{
		{"Search Tesla news", `{"query":"Tesla news"}`, "magnifyingglass"},
		{"Run npm cache clean", `{"command":"npm cache clean --force"}`, "terminal"},
		{"Fetch the page", `{"url":"https://eve.dev"}`, "globe"},
		{"Roll dice", `{"count":2}`, "wrench.and.screwdriver"},
	} {
		row := p.ToolRow(agent.ToolCall{Name: c.title, Input: json.RawMessage(c.input)})
		if row.Name != c.title || row.Icon != c.icon || row.Labels == nil || row.Labels.Completed != c.title {
			t.Fatalf("%s: %+v %+v", c.title, row, row.Labels)
		}
	}
	if row := p.ToolRow(agent.ToolCall{}); row.Name != "" {
		t.Fatalf("unnamed call labeled: %+v", row)
	}
}
