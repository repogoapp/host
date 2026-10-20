// Package catalog is what an environment or a project offers to call, as text a
// voice session reads: the agents a chat can start with and whether each can
// run, then plugins, MCP tools and the host's own tools, named as a card names them.
package catalog

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/repogoapp/host/internal/agentcatalog"
	"github.com/repogoapp/host/internal/clitool"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/hostsetup"
	"github.com/repogoapp/host/internal/mcp"
	"github.com/repogoapp/host/internal/plugins"
	"github.com/repogoapp/host/internal/projectagent"
	"github.com/repogoapp/host/internal/rpc"
)

// Parts is what one read gathered, each part by its own helper. An Err is why
// a part is missing: it failed, or didn't answer in time.
type Parts struct {
	Agents           []hostsetup.Agent
	AgentsErr        error
	ProjectAgents    []projectagent.ProjectAgent
	ProjectAgentsErr error
	Plugins          []plugins.Listed
	PluginsErr       error
	Servers          []mcp.ServerTools
	ServersErr       error
	HostTools        []rpc.ToolDef
}

// inlineTools is the most tools a server lists in the summary; a bigger one
// is a line with its count, and Detail names them all.
const inlineTools = 15

// briefLength caps a description in the summary, cut at a word.
const briefLength = 140

// modelsPerAgent caps an agent's models: a router agent offers hundreds, more
// than a voice prompt holds; a model named past them still resolves.
const modelsPerAgent = 12

// Render is the summary: every section that has something, in a fixed order.
func Render(p Parts) string {
	var b strings.Builder
	agentsSection(&b, "Agents", p.Agents, p.AgentsErr)
	projectAgentsSection(&b, p.ProjectAgents, p.ProjectAgentsErr)
	pluginsSection(&b, p.Plugins, p.PluginsErr)
	serversSection(&b, p.Servers, p.ServersErr)
	hostToolsSection(&b, p.HostTools)
	return b.String()
}

// Detail is one plugin (`plans` or `plugin:plans`) or one MCP server
// (`mcp:linear`) in full: every method or tool, with its whole description.
func Detail(p Parts, name string) (string, error) {
	var b strings.Builder
	if key, ok := strings.CutPrefix(name, "mcp:"); ok {
		if p.ServersErr != nil {
			return "", fmt.Errorf("catalog: couldn't list the MCP servers: %w", p.ServersErr)
		}
		i := slices.IndexFunc(p.Servers, func(s mcp.ServerTools) bool { return s.Server == key })
		if i < 0 {
			return "", errkind.ErrNotFound.Errorf("catalog: no MCP server %q is on here; there are %s", key, serverKeys(p.Servers))
		}
		s := p.Servers[i]
		fmt.Fprintf(&b, "%s (mcp:%s):\n", s.Label, s.Server)
		for _, t := range s.Tools {
			b.WriteString(toolLine("", s.Server, t, false))
		}
		return b.String(), nil
	}
	if p.PluginsErr != nil {
		return "", fmt.Errorf("catalog: couldn't list the plugins: %w", p.PluginsErr)
	}
	plugin := strings.TrimPrefix(name, "plugin:")
	i := slices.IndexFunc(p.Plugins, func(l plugins.Listed) bool { return l.Name == plugin })
	if i < 0 {
		return "", errkind.ErrNotFound.Errorf("catalog: there's no plugin %q here; there are %s", plugin, pluginNames(p.Plugins))
	}
	l := p.Plugins[i]
	fmt.Fprintf(&b, "%s%s\n", l.Name, describedAs(l.Description, false))
	for _, m := range l.Methods {
		b.WriteString(methodLine("", m, false))
	}
	return b.String(), nil
}

func agentsSection(b *strings.Builder, heading string, agents []hostsetup.Agent, err error) {
	if len(agents) == 0 && err == nil {
		return
	}
	fmt.Fprintf(b, "%s:\n", heading)
	if err != nil {
		fmt.Fprintf(b, "- Couldn't check them: %s\n", err)
		return
	}
	for _, a := range agents {
		name := cmp.Or(a.Catalog.Name, a.Install.Name, a.Install.Kind)
		fmt.Fprintf(b, "- %s (%s): %s.", name, a.Install.Kind, agentState(a.Install))
		if a.Install.Installed && a.Install.Authed {
			b.WriteString(modelsText(environmentModels(a.Catalog), a.Catalog.DefaultModel))
		}
		b.WriteString("\n")
	}
}

func projectAgentsSection(b *strings.Builder, agents []projectagent.ProjectAgent, err error) {
	if len(agents) == 0 && err == nil {
		return
	}
	b.WriteString("Project agents:\n")
	if err != nil {
		fmt.Fprintf(b, "- Couldn't list them: %s\n", err)
		return
	}
	for _, a := range agents {
		if a.Error != "" {
			fmt.Fprintf(b, "- %s (%s): broken: %s\n", a.Name, a.ID, a.Error)
			continue
		}
		var models []model
		for _, m := range a.Models {
			models = append(models, model{id: m.ID, name: m.Name})
		}
		fmt.Fprintf(b, "- %s (%s): ready.%s\n", a.Name, a.ID, modelsText(models, a.Model))
	}
}

func pluginsSection(b *strings.Builder, listed []plugins.Listed, err error) {
	if len(listed) == 0 && err == nil {
		return
	}
	b.WriteString("Plugins:\n")
	if err != nil {
		fmt.Fprintf(b, "- Couldn't list them: %s\n", err)
		return
	}
	for _, l := range listed {
		fmt.Fprintf(b, "- %s, %s%s\n", l.Name, pluginDir(l.Dir), describedAs(l.Description, true))
		for _, m := range l.Methods {
			b.WriteString(methodLine("  ", m, true))
		}
	}
}

func serversSection(b *strings.Builder, servers []mcp.ServerTools, err error) {
	if len(servers) == 0 && err == nil {
		return
	}
	b.WriteString("MCP servers:\n")
	if err != nil {
		fmt.Fprintf(b, "- Couldn't list them: %s\n", err)
		return
	}
	for _, s := range servers {
		switch {
		case s.Error != "":
			fmt.Fprintf(b, "- %s (mcp:%s): couldn't list its tools: %s\n", s.Label, s.Server, s.Error)
		case len(s.Tools) > inlineTools:
			fmt.Fprintf(b, "- %s (mcp:%s): %d tools; name \"mcp:%s\" for each one.\n", s.Label, s.Server, len(s.Tools), s.Server)
		default:
			fmt.Fprintf(b, "- %s (mcp:%s):\n", s.Label, s.Server)
			for _, t := range s.Tools {
				b.WriteString(toolLine("  ", s.Server, t, true))
			}
		}
	}
}

func hostToolsSection(b *strings.Builder, tools []rpc.ToolDef) {
	if len(tools) == 0 {
		return
	}
	b.WriteString("RepoGo:\n")
	for _, t := range tools {
		schema, _ := json.Marshal(t.InputSchema)
		fmt.Fprintf(b, "- repogo:%s(%s)%s\n", t.Method, plugins.Signature(schema), describedAs(t.Description, true))
	}
}

// methodLine writes a method by its full name (`plans.move`), which already
// starts with its plugin's, so the line is the name a call takes.
func methodLine(indent string, m plugins.ListedMethod, brief bool) string {
	line := fmt.Sprintf("%s- plugin:%s(%s)%s", indent, m.Name, m.Input, describedAs(m.Description, brief))
	if m.Asks {
		line += " Asks first."
	}
	return line + "\n"
}

func toolLine(indent, server string, t mcp.ToolInfo, brief bool) string {
	line := fmt.Sprintf("%s- mcp:%s.%s(%s)%s", indent, server, t.Name, plugins.Signature(t.Input), describedAs(t.Description, brief))
	if t.AsksFirst {
		line += " Asks first."
	}
	return line + "\n"
}

// agentState is whether a chat can start with an agent CLI now.
func agentState(install clitool.Install) string {
	switch {
	case !install.Installed:
		return "not installed"
	case !install.Authed:
		return "not signed in"
	default:
		return "ready"
	}
}

func pluginDir(dir string) string {
	if dir == "env" {
		return "the environment's"
	}
	return "this project's"
}

// describedAs is ": " and a description, or nothing; brief keeps its first
// sentence, cut at a word.
func describedAs(description string, brief bool) string {
	d := strings.Join(strings.Fields(description), " ")
	if d == "" {
		return ""
	}
	if brief {
		d = firstSentence(d)
	}
	if strings.HasSuffix(d, "…") {
		return ": " + d
	}
	return ": " + strings.TrimRight(d, ".") + "."
}

func firstSentence(d string) string {
	if i := strings.Index(d, ". "); i >= 0 {
		d = d[:i]
	}
	if len(d) <= briefLength {
		return d
	}
	cut := strings.LastIndex(d[:briefLength], " ")
	if cut <= 0 {
		cut = briefLength
	}
	return d[:cut] + "…"
}

type model struct{ id, name string }

// environmentModels is what the CLI's own picker offers, hidden ones left out.
func environmentModels(c agentcatalog.Catalog) []model {
	var out []model
	for _, m := range c.Models {
		if !m.Hidden {
			out = append(out, model{id: m.ID, name: m.Name})
		}
	}
	return out
}

func modelsText(models []model, defaultModel string) string {
	if len(models) == 0 {
		return ""
	}
	var names []string
	for _, m := range models[:min(len(models), modelsPerAgent)] {
		name := m.id
		if m.name != "" && m.name != m.id {
			name = m.name + " (" + m.id + ")"
		}
		names = append(names, name)
	}
	if more := len(models) - len(names); more > 0 {
		names = append(names, fmt.Sprintf("and %d more", more))
	}
	text := " Models: " + strings.Join(names, ", ") + "."
	if defaultModel != "" {
		text += " Default: " + defaultModel + "."
	}
	return text
}

func serverKeys(servers []mcp.ServerTools) string {
	keys := make([]string, len(servers))
	for i, s := range servers {
		keys[i] = "mcp:" + s.Server
	}
	return orNone(keys)
}

func pluginNames(listed []plugins.Listed) string {
	names := make([]string, len(listed))
	for i, l := range listed {
		names[i] = l.Name
	}
	return orNone(names)
}

func orNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
