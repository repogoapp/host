package repogomcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/plugins"
)

// pluginsProject is a project folder with a .repogo/plugins folder in it.
func pluginsProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, plugins.Folder), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// pluginsService runs plugins from sdk; "" is a host without the SDK.
func pluginsService(t *testing.T, sdk string) *plugins.Service {
	t.Helper()
	home, err := os.MkdirTemp("", "rgm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	return plugins.New(t.Context(), plugins.Config{
		SDK: sdk, Socket: filepath.Join(home, "plugins.sock"), DataDir: filepath.Join(home, "data"),
		Emit: func(device.ID, emit.Event) error { return nil }, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

func pluginsServer(t *testing.T, service *plugins.Service) *Server {
	t.Helper()
	return New(Config{
		URL: func() string { return "http://127.0.0.1:1/mcp/repogo" }, On: func(string) bool { return true },
		Browser: newFakePhone(true, nil).browser, Plugins: service,
	})
}

func attachIn(t *testing.T, s *Server, cwd string) string {
	t.Helper()
	turn := agent.RunningTurn{Ctx: context.Background(), TurnID: "t1", ChatID: "claude:s1", Cwd: cwd, Device: "phone"}
	entry, release := s.Attach(&fakeSession{turn: turn}, cwd)
	t.Cleanup(release)
	return strings.TrimPrefix(entry.Headers[0].Value, "Bearer ")
}

func toolNames(t *testing.T, s *Server, token string) string {
	t.Helper()
	w := post(t, s, token, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	var got struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range got.Result.Tools {
		names = append(names, tool.Name)
	}
	return strings.Join(names, ",")
}

// A host without the SDK (every release) offers neither tool, even in a
// project with plugins, and refuses a call by name.
func TestPluginsToolsNeedTheSDK(t *testing.T) {
	root := pluginsProject(t)
	s := pluginsServer(t, pluginsService(t, ""))
	token := attachIn(t, s, root)
	if got := toolNames(t, s, token); got != "browser" {
		t.Fatalf("tools without the SDK: %q", got)
	}
	if got := callTool(t, s, token, "plugins", map[string]any{}); !got.IsError || !strings.Contains(got.Content[0].Text, "unknown tool") {
		t.Fatalf("call without the SDK: %+v", got)
	}
}

// With the SDK, only a project with a .repogo/plugins folder gets them.
func TestPluginsToolsFollowTheProjectFolder(t *testing.T) {
	s := pluginsServer(t, pluginsService(t, "/sdk"))
	if got := toolNames(t, s, attachIn(t, s, pluginsProject(t))); got != "browser,plugins,plugin_call" {
		t.Fatalf("project with plugins: %q", got)
	}
	if got := toolNames(t, s, attachIn(t, s, t.TempDir())); got != "browser" {
		t.Fatalf("project without plugins: %q", got)
	}
}

// A failed call keeps the plugin's code and field errors for the agent to act on.
func TestPluginResultKeepsThePluginsErrorShape(t *testing.T) {
	got := pluginResult(plugins.Answer{Error: &plugins.Failure{Code: "VALIDATION", Message: "Check the highlighted fields.", FieldErrors: map[string]string{"/status": "Invalid option"}}}, nil)
	if !got.IsError || !strings.Contains(got.Content[0].Text, `"/status"`) {
		t.Fatalf("%+v", got)
	}
	got = pluginResult(plugins.Answer{Result: json.RawMessage(`{"id":"p1"}`)}, nil)
	if got.IsError || !strings.Contains(got.Content[0].Text, `"p1"`) {
		t.Fatalf("%+v", got)
	}
}

// End to end through the plugins server and this repo's SDK.
func TestPluginCallRunsTheProjectsPlugin(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun is not installed")
	}
	sdk, err := filepath.Abs("../../plugins-sdk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(sdk, "node_modules", "zod")); err != nil {
		t.Skip("plugins-sdk has no node_modules; run bun install there")
	}
	root := pluginsProject(t)
	dir := filepath.Join(root, plugins.Folder)
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "@repogo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sdk, filepath.Join(dir, "node_modules", "@repogo", "plugins")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	plugin := `import { definePlugin, z } from "@repogo/plugins";
export default definePlugin({ collections: { notes: z.object({ text: z.string() }) } });
`
	if err := os.WriteFile(filepath.Join(dir, "notes", "app.ts"), []byte(plugin), 0o644); err != nil {
		t.Fatal(err)
	}
	s := pluginsServer(t, pluginsService(t, sdk))
	token := attachIn(t, s, root)

	if got := callTool(t, s, token, "plugins", map[string]any{}); got.IsError || !strings.Contains(got.Content[0].Text, "notes.notes.create") {
		t.Fatalf("plugins: %+v", got)
	}
	created := callTool(t, s, token, "plugin_call", map[string]any{"method": "notes.notes.create", "input": map[string]any{"text": "hi"}})
	if created.IsError || !strings.Contains(created.Content[0].Text, `"hi"`) {
		t.Fatalf("create: %+v", created)
	}
	bad := callTool(t, s, token, "plugin_call", map[string]any{"method": "notes.notes.create", "input": map[string]any{}})
	if !bad.IsError || !strings.Contains(bad.Content[0].Text, "VALIDATION") {
		t.Fatalf("bad input: %+v", bad)
	}
}
