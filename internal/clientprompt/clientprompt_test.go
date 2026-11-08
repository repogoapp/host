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

var update = flag.Bool("update", false, "rewrite cards.md from the cards package's prompt.md, and testdata/app.md")

const cardsPrompt = "../../../swift/Packages/RepoGoCards/Tests/RepoGoCardsTests/Fixtures/prompt.md"

func TestForPicksByRenderingFeature(t *testing.T) {
	cases := []struct {
		feature string
		marker  string
	}{
		{"markdown", "- Files and folders:"},
		{"mermaid", "```mermaid"},
		{"math", "# Math"},
		{"cards", "Use these tags:"},
	}
	for _, tc := range cases {
		t.Run(tc.feature, func(t *testing.T) {
			client := device.Client{Features: map[string]int{tc.feature: 1}}
			prompt := clientprompt.For(client, clientprompt.NoPlugins, nil)
			for _, other := range cases {
				if got := strings.Contains(prompt, other.marker); got != (other.feature == tc.feature) {
					t.Errorf("%s instructions present = %v", other.feature, got)
				}
			}
			client.Features[tc.feature] = 9
			if got := clientprompt.For(client, clientprompt.NoPlugins, nil); got != prompt {
				t.Error("newer capability version does not get the newest prompt")
			}
			client.Features[tc.feature] = 0
			if got := clientprompt.For(client, clientprompt.NoPlugins, nil); got != "" {
				t.Errorf("disabled capability produced %q", got)
			}
		})
	}
	for _, features := range []map[string]int{nil, {"blocks": 3}} {
		if got := clientprompt.For(device.Client{Features: features}, clientprompt.NoPlugins, nil); got != "" {
			t.Errorf("unknown capabilities produced %q", got)
		}
	}
}

func TestClipDoesNotReceiveUnsupportedInstructions(t *testing.T) {
	clip := device.Client{Features: map[string]int{"markdown": 1}}
	prompt := clientprompt.For(clip, clientprompt.HasPlugins, []string{"linear"})
	for _, marker := range []string{"```mermaid", "# Math", "```ui", "Use these tags:", "MCP servers for cards:"} {
		if strings.Contains(prompt, marker) {
			t.Errorf("Clip receives unsupported instructions: %s", marker)
		}
	}
	if !strings.Contains(prompt, "# Replying in RepoGo") || !strings.Contains(prompt, "short recap") {
		t.Error("Clip lacks shared response rules")
	}
	if !strings.Contains(prompt, "- Files and folders:") {
		t.Error("Clip lacks basic Markdown instructions")
	}
}

func TestBasicCardsDoNotTeachUnavailableDataSources(t *testing.T) {
	c := device.Client{Features: map[string]int{"markdown": 1, "cards": 1}}
	prompt := clientprompt.For(c, clientprompt.NoPlugins, nil)
	for _, marker := range []string{`method="mcp:`, `action="plugin:`} {
		if strings.Contains(prompt, marker) {
			t.Errorf("basic cards include an unavailable data source: %s", marker)
		}
	}
}

func TestCardSourcesRequireTheCardRenderer(t *testing.T) {
	c := device.Client{Features: map[string]int{"plugins": 1, "mcp": 1, "repogo": 1}}
	if got := clientprompt.For(c, clientprompt.HasPlugins, []string{"linear"}); got != "" {
		t.Errorf("card sources without a renderer produced %q", got)
	}
}

func TestSectionsDescribeTheAssembledPrompt(t *testing.T) {
	c := device.Client{Features: map[string]int{"markdown": 1, "cards": 1, "mcp": 1}}
	sections := clientprompt.Sections(c, clientprompt.NoPlugins, []string{"linear"})
	var parts []string
	seen := map[string]bool{}
	for _, section := range sections {
		if seen[section.ID] || section.ID == "" || section.Title == "" || section.Reason == "" || section.Text == "" {
			t.Fatalf("incomplete or duplicate section: %+v", section)
		}
		seen[section.ID] = true
		if _, err := os.Stat(strings.TrimPrefix(section.File, "internal/clientprompt/")); err != nil {
			t.Fatal(err)
		}
		if section.Included {
			parts = append(parts, section.Text)
		}
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".md") && !seen[strings.TrimSuffix(entry.Name(), ".md")] {
			t.Errorf("prompt file has no section: %s", entry.Name())
		}
	}
	if got := strings.Join(parts, "\n"); got != clientprompt.For(c, clientprompt.NoPlugins, []string{"linear"}) {
		t.Error("inspector sections differ from the agent prompt")
	}
	if !strings.Contains(sections[2].Reason, "Missing: mermaid capability") {
		t.Errorf("missing renderer not explained: %s", sections[2].Reason)
	}
}

// The card grammar is the cards package's (`Fixtures/prompt.md`), which every
// renderer is tested against; cards.md is its copy. The other card sections,
// RepoGo's own sources, are this package's.
func TestCardsMatchThePackage(t *testing.T) {
	testhost.SkipWithoutClient(t, "../../../swift")
	src, err := os.ReadFile(cardsPrompt)
	if err != nil {
		t.Fatal(err)
	}
	testhost.Golden(t, "cards.md", []byte(strings.TrimRight(string(src), "\n")+"\n"), *update,
		"run: go test ./internal/clientprompt -update")
}

// Only the app opens a Connect's sheet, so only it is told to draw one,
// ahead of the card elements.
func TestConnectComesToTheAppFirst(t *testing.T) {
	const rule = "Never ask for a key"
	app := clientprompt.For(device.Client{Platform: "ios", Features: map[string]int{"markdown": 1, "cards": 1, "repogo": 1}}, clientprompt.NoPlugins, nil)
	if i := strings.Index(app, rule); i < 0 || i > strings.Index(app, "Use these tags:") {
		t.Errorf("the app's prompt does not lead with the Connect rule:\n%s", app)
	}
	clip := clientprompt.For(device.Client{Platform: "ios", Features: map[string]int{"markdown": 1}}, clientprompt.NoPlugins, nil)
	if strings.Contains(clip, rule) {
		t.Error("the Clip, which can't open a Connect, is told to draw one")
	}
}

// Building plugins, and drawing them, comes to a client whose cards reach them
// on an environment that runs them: the first plugin's page needs plugin cards.
func TestPluginsPromptsFollowTheProjectAndTheClient(t *testing.T) {
	both := device.Client{Platform: "ios", Features: map[string]int{"markdown": 1, "cards": 1, "plugins": 1}}
	clip := device.Client{Platform: "ios", Features: map[string]int{"markdown": 1}}
	const build, draw = "~/.repogo/plugins/<name>/app.ts", "plugin:<plugin>.<method>"
	for _, c := range []struct {
		name        string
		client      device.Client
		plugins     clientprompt.Plugins
		build, draw bool
	}{
		{"an environment with plugins", both, clientprompt.HasPlugins, true, true},
		{"an environment without plugins", both, clientprompt.CanAddPlugins, true, true},
		{"an environment without the runtime", both, clientprompt.NoPlugins, false, false},
		{"a client without plugin cards", clip, clientprompt.HasPlugins, false, false},
	} {
		prompt := clientprompt.For(c.client, c.plugins, nil)
		// Plugins live only on the environment: no prompt teaches writing one into a project.
		if strings.Contains(strings.ReplaceAll(prompt, "~/.repogo/plugins/<name>/app.ts", ""), ".repogo/plugins/<name>/app.ts") {
			t.Errorf("%s: the prompt teaches a project plugin", c.name)
		}
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
	app := device.Client{Platform: "ios", Features: map[string]int{"markdown": 1, "cards": 1, "plugins": 1, "mcp": 1}}
	older := device.Client{Platform: "ios", Features: map[string]int{"markdown": 1, "cards": 1, "plugins": 1}}
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
	app := device.Client{Platform: "ios", Features: map[string]int{"markdown": 1, "cards": 1, "plugins": 1, "mcp": 1, "repogo": 1}}
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
	app := device.Client{Platform: "ios", Features: map[string]int{"markdown": 1, "mermaid": 1, "math": 1, "cards": 1, "plugins": 1, "mcp": 1, "repogo": 1}}
	prompt := repogomcp.ChatsPrompt(false) + clientprompt.For(app, clientprompt.HasPlugins, []string{"linear", "vercel"})
	testhost.Golden(t, "testdata/app.md", []byte(prompt), *update, "run: go test ./internal/clientprompt -update")
}
