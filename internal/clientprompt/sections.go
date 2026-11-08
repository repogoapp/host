package clientprompt

import (
	"embed"
	"strings"

	"github.com/repogoapp/host/internal/device"
)

//go:embed *.md
var files embed.FS

// Section is one source and its selection result, shared by the prompt and inspector.
type Section struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	File     string `json:"file"`
	Included bool   `json:"included"`
	Reason   string `json:"reason"`
	Text     string `json:"text"`
}

type requirement struct {
	met    bool
	reason string
}

type sectionSpec struct {
	file     string
	title    string
	requires []requirement
}

// Sections lists every section in prompt order, including why each is selected.
func Sections(c device.Client, plugins Plugins, servers []string) []Section {
	feature := func(name string) requirement {
		return requirement{c.Features[name] >= 1, name + " capability >= 1"}
	}
	cards := feature("cards")
	pluginCards := cards.met && c.Features["plugins"] >= 1 && plugins != NoPlugins
	mcpCards := cards.met && c.Features["mcp"] >= 1 && len(servers) > 0
	hostCards := cards.met && c.Features["repogo"] >= 1
	runtime := requirement{plugins != NoPlugins, "plugin runtime available"}
	installed := requirement{plugins == HasPlugins, "environment has plugins"}
	connected := requirement{len(servers) > 0, "project has enabled MCP servers"}
	data := requirement{(pluginCards && installed.met) || mcpCards || hostCards, "at least one available card data source"}
	specs := []sectionSpec{
		{"response.md", "Response rules", []requirement{feature("markdown")}},
		{"markdown.md", "Markdown", []requirement{feature("markdown")}},
		{"mermaid.md", "Mermaid", []requirement{feature("mermaid")}},
		{"math.md", "Math", []requirement{feature("math")}},
		{"cards_connect.md", "Keys and sign-in", []requirement{cards, feature("repogo")}},
		{"cards.md", "Cards", []requirement{cards}},
		{"apps.md", "Building plugins and pages", []requirement{cards, feature("plugins"), runtime}},
		{"cards_data.md", "Card data", []requirement{cards, data}},
		{"cards_plugins.md", "Plugin cards", []requirement{cards, feature("plugins"), runtime}},
		{"cards_repogo.md", "RepoGo cards", []requirement{cards, feature("repogo")}},
		{"cards_mcp.md", "MCP cards", []requirement{cards, feature("mcp"), connected}},
	}
	result := make([]Section, 0, len(specs))
	for _, spec := range specs {
		body, err := files.ReadFile(spec.file)
		if err != nil {
			panic(err)
		}
		section := Section{
			ID: strings.TrimSuffix(spec.file, ".md"), Title: spec.title,
			File: "internal/clientprompt/" + spec.file, Included: true, Text: string(body),
		}
		var reasons, missing []string
		for _, rule := range spec.requires {
			reasons = append(reasons, rule.reason)
			if !rule.met {
				missing = append(missing, rule.reason)
			}
		}
		section.Reason = "Requires: " + strings.Join(reasons, "; ") + "."
		if len(missing) > 0 {
			section.Included = false
			section.Reason = "Missing: " + strings.Join(missing, "; ") + "."
		}
		if spec.file == "cards_mcp.md" && section.Included {
			section.Text += "MCP servers for cards: `" + strings.Join(servers, "`, `") + "`.\n"
		}
		result = append(result, section)
	}
	return result
}
