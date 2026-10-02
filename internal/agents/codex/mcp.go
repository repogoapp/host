package codex

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/mcp"
)

// codexSection matches a `[mcp_servers.<name>]` header, bare or quoted.
var codexSection = regexp.MustCompile(`^\[\s*mcp_servers\.(?:"([^"]+)"|'([^']+)'|([A-Za-z0-9_-]+))\s*\]$`)

// codexInstalled reads `[mcp_servers.<name>]` tables out of a Codex config.
// Only the keys worth showing are read, so a TOML parser is not worth its
// weight; a value this cannot read leaves the entry's target blank.
func codexInstalled(path, scope string) []mcp.Installed {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	type entry struct {
		name, url, command string
		args               []string
		disabled           bool
	}
	var entries []*entry
	var cur *entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			cur = nil
			if m := codexSection.FindStringSubmatch(line); m != nil {
				cur = &entry{name: m[1] + m[2] + m[3]}
				entries = append(entries, cur)
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if cur == nil || !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "url":
			cur.url = tomlString(value)
		case "command":
			cur.command = tomlString(value)
		case "args":
			for _, a := range strings.Split(strings.Trim(value, "[]"), ",") {
				if s := tomlString(strings.TrimSpace(a)); s != "" {
					cur.args = append(cur.args, s)
				}
			}
		case "enabled":
			cur.disabled = value == "false"
		}
	}
	var out []mcp.Installed
	for _, e := range entries {
		if e.disabled {
			continue
		}
		transport := "stdio"
		if e.url != "" {
			transport = "http"
		}
		out = append(out, mcp.Installed{Name: e.name, Agent: string(agent.KindCodex), Scope: scope, Transport: transport, Target: mcp.Target(e.url, e.command, e.args)})
	}
	return out
}

// tomlString is a quoted TOML string's contents, or empty for anything else.
func tomlString(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return ""
}

// InstalledMCP is what Codex already loads for root: the user's config.toml
// and the project's .codex/config.toml. A file that is missing or unreadable
// contributes nothing.
func (p *Provider) InstalledMCP(root string) []mcp.Installed {
	out := codexInstalled(filepath.Join(p.home, "config.toml"), "user")
	if root != "" {
		out = append(out, codexInstalled(filepath.Join(root, ".codex", "config.toml"), "project")...)
	}
	return out
}
