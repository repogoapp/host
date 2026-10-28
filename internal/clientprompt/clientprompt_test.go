package clientprompt_test

import (
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/clientprompt"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/repogomcp"
	"github.com/repogoapp/host/internal/testhost"
)

var update = flag.Bool("update", false, "rewrite cards.md from the phone's CardInstructions, and testdata/app.md")

const cardInstructions = "../../../swift/Packages/RepoGoCards/Sources/RepoGoCards/CardInstructions.swift"

func TestForPicksByMarkdownFeature(t *testing.T) {
	prompt := func(features map[string]int) string {
		return clientprompt.For(device.Client{Platform: "ios", Features: features}, clientprompt.NoPlugins, nil)
	}
	full := prompt(map[string]int{"markdown": 1})
	if !strings.Contains(full, "```mermaid") || !strings.Contains(full, "```ui") {
		t.Fatalf("markdown 1 does not describe Mermaid and cards:\n%s", full)
	}
	if got := prompt(map[string]int{"markdown": 9}); got != full {
		t.Error("a newer markdown version does not get the newest prompt")
	}
	for _, features := range []map[string]int{nil, {"blocks": 3}, {"markdown": 0}} {
		if got := prompt(features); got != "" {
			t.Errorf("For(%v) = %q, want nothing", features, got)
		}
	}
}

// The card prompts are the phone's: the phone renders the cards, so its
// CardInstructions is the source and cards.md and cards_plugins.md copies.
func TestCardsMatchThePhone(t *testing.T) {
	testhost.SkipWithoutClient(t, "../../../swift")
	src, err := os.ReadFile(cardInstructions)
	if err != nil {
		t.Fatal(err)
	}
	for file, start := range map[string]string{
		"cards.md":         "public static let prompt = #\"\"\"\n",
		"cards_connect.md": "public static let connect = #\"\"\"\n",
		"cards_data.md":    "public static let data = #\"\"\"\n",
		"cards_plugins.md": "public static let plugins = #\"\"\"\n",
		"cards_mcp.md":     "public static let mcp = #\"\"\"\n",
		"cards_repogo.md":  "public static let repogo = #\"\"\"\n",
		"apps.md":          "public static let apps = #\"\"\"\n",
	} {
		_, body, ok := strings.Cut(string(src), start)
		body, _, closed := strings.Cut(body, "\n    \"\"\"#")
		if !ok || !closed {
			t.Fatalf("no %q … \"\"\"# string in %s", start, cardInstructions)
		}
		lines := strings.Split(body, "\n")
		for i, line := range lines {
			lines[i] = strings.TrimPrefix(line, "    ")
			if strings.TrimSpace(lines[i]) == "" {
				lines[i] = ""
			}
		}
		testhost.Golden(t, file, []byte(strings.Join(lines, "\n")+"\n"), *update,
			"run: go test ./internal/clientprompt -update")
	}
}

// Only the app opens a Connect's sheet, so only it is told to draw one,
// ahead of the card elements.
func TestConnectComesToTheAppFirst(t *testing.T) {
	const rule = "Never ask\nfor one in chat"
	app := clientprompt.For(device.Client{Platform: "ios", Features: map[string]int{"markdown": 1, "repogo": 1}}, clientprompt.NoPlugins, nil)
	if i := strings.Index(app, rule); i < 0 || i > strings.Index(app, "Use these tags:") {
		t.Errorf("the app's prompt does not lead with the Connect rule:\n%s", app)
	}
	clip := clientprompt.For(device.Client{Platform: "ios", Features: map[string]int{"markdown": 1}}, clientprompt.NoPlugins, nil)
	if strings.Contains(clip, rule) {
		t.Error("the Clip, which can't open a Connect, is told to draw one")
	}
}

// Building plugins comes to a client whose cards reach them, on an
// environment that runs them; drawing them, only once the project has some.
func TestPluginsPromptsFollowTheProjectAndTheClient(t *testing.T) {
	both := device.Client{Platform: "ios", Features: map[string]int{"markdown": 1, "plugins": 1}}
	clip := device.Client{Platform: "ios", Features: map[string]int{"markdown": 1}}
	const build, draw = ".repogo/plugins/<name>/app.ts", "plugin:<plugin>.<method>"
	for _, c := range []struct {
		name        string
		client      device.Client
		plugins     clientprompt.Plugins
		build, draw bool
	}{
		{"a project with plugins", both, clientprompt.HasPlugins, true, true},
		{"a project without plugins", both, clientprompt.CanAddPlugins, true, false},
		{"an environment without the runtime", both, clientprompt.NoPlugins, false, false},
		{"a client without plugin cards", clip, clientprompt.HasPlugins, false, false},
	} {
		prompt := clientprompt.For(c.client, c.plugins, nil)
		if got := strings.Contains(prompt, build); got != c.build {
			t.Errorf("%s: building prompt = %v, want %v", c.name, got, c.build)
		}
		if got := strings.Contains(prompt, draw); got != c.draw {
			t.Errorf("%s: plugin cards prompt = %v, want %v", c.name, got, c.draw)
		}
	}
}

// MCP cards come to a client that draws them, for a project with a server on,
// with the data elements whether or not the project has plugins.
func TestMCPPromptFollowsTheProjectAndTheClient(t *testing.T) {
	app := device.Client{Platform: "ios", Features: map[string]int{"markdown": 1, "plugins": 1, "mcp": 1}}
	older := device.Client{Platform: "ios", Features: map[string]int{"markdown": 1, "plugins": 1}}
	const data, draw = "Data: name, method=", "mcp:<server>.<tool>"
	for _, c := range []struct {
		name       string
		client     device.Client
		plugins    clientprompt.Plugins
		servers    []string
		data, draw bool
	}{
		{"servers on, no plugins", app, clientprompt.NoPlugins, []string{"neon"}, true, true},
		{"servers on and plugins", app, clientprompt.HasPlugins, []string{"neon"}, true, true},
		{"no servers on", app, clientprompt.CanAddPlugins, nil, false, false},
		{"a client without MCP cards", older, clientprompt.CanAddPlugins, []string{"neon"}, false, false},
	} {
		prompt := clientprompt.For(c.client, c.plugins, c.servers)
		if got := strings.Contains(prompt, data); got != c.data {
			t.Errorf("%s: data prompt = %v, want %v", c.name, got, c.data)
		}
		if got := strings.Contains(prompt, draw); got != c.draw {
			t.Errorf("%s: MCP cards prompt = %v, want %v", c.name, got, c.draw)
		}
		if strings.Count(prompt, data) > 1 {
			t.Errorf("%s: the data prompt appears twice", c.name)
		}
	}
	prompt := clientprompt.For(app, clientprompt.NoPlugins, []string{"neon", "vercel"})
	if !strings.HasSuffix(prompt, "MCP servers for cards: `neon`, `vercel`.\n") {
		t.Errorf("the prompt doesn't end naming the servers:\n%s", prompt[len(prompt)-200:])
	}
}

// The chats come to a client whose cards reach the host's methods, with the
// data elements, in any project; the MCP servers' line still ends the prompt.
func TestRepoGoPromptFollowsTheClient(t *testing.T) {
	app := device.Client{Platform: "ios", Features: map[string]int{"markdown": 1, "plugins": 1, "mcp": 1, "repogo": 1}}
	clip := device.Client{Platform: "ios", Features: map[string]int{"markdown": 1}}
	const data, draw = "Data: name, method=", `method="repogo:<method>"`
	prompt := clientprompt.For(app, clientprompt.NoPlugins, []string{"neon"})
	if !strings.Contains(prompt, data) || !strings.Contains(prompt, draw) {
		t.Errorf("the app's prompt lacks the chats")
	}
	if !strings.HasSuffix(prompt, "MCP servers for cards: `neon`.\n") {
		t.Errorf("the prompt doesn't end naming the servers")
	}
	if strings.Contains(clientprompt.For(clip, clientprompt.NoPlugins, nil), draw) {
		t.Errorf("the Clip is taught repogo: cards")
	}
}

// TestWritesTheAppsPrompt keeps what an agent started from the app gets added
// to its system prompt, joined as host.New's Instructions joins it, in a
// project with plugins and MCP servers, so a change to it reads as a diff.
func TestWritesTheAppsPrompt(t *testing.T) {
	app := device.Client{Platform: "ios", Features: map[string]int{"markdown": 1, "plugins": 1, "mcp": 1, "repogo": 1}}
	prompt := repogomcp.ChatsPrompt(false) + clientprompt.For(app, clientprompt.HasPlugins, []string{"linear", "vercel"})
	testhost.Golden(t, "testdata/app.md", []byte(prompt), *update, "run: go test ./internal/clientprompt -update")
}
