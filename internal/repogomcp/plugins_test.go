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
	"github.com/repogoapp/host/internal/emit/emittest"
	"github.com/repogoapp/host/internal/outcome"
	"github.com/repogoapp/host/internal/plugins"
)

// pluginsService runs plugins from sdk's source; "" is a host without a runtime.
func pluginsService(t *testing.T, sdk string) *plugins.Service {
	t.Helper()
	return pluginsServiceIn(t, sdk, "")
}

// pluginsServiceIn runs the plugins in global, the environment's folder.
func pluginsServiceIn(t *testing.T, sdk, global string) *plugins.Service {
	t.Helper()
	home, err := os.MkdirTemp("", "rgm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	return plugins.New(t.Context(), plugins.Config{
		Runtime: runtimeOf(sdk), Socket: filepath.Join(home, "plugins.sock"), DataDir: filepath.Join(home, "data"), Global: global,
		Events: emittest.Sink{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// runtimeOf is the SDK's source entry, or "" for none.
func runtimeOf(sdk string) string {
	if sdk == "" {
		return ""
	}
	return filepath.Join(sdk, "src", "run.ts")
}

func pluginsServer(t *testing.T, service *plugins.Service) *Server {
	t.Helper()
	return New(Config{
		URL: func() string { return "http://127.0.0.1:1/mcp/repogo" }, On: func(string) bool { return true },
		Browser: newFakePhone(true, nil).browser, Plugins: service, MCP: mcpServiceFunc(t),
	})
}

func attachIn(t *testing.T, s *Server, cwd string) string {
	t.Helper()
	return attachAsking(t, s, cwd, agent.TurnConfig{}, nil)
}

// attachAsking attaches a turn run with cfg whose chat answers approvals with ask.
func attachAsking(t *testing.T, s *Server, cwd string, cfg agent.TurnConfig, ask func(context.Context, agent.Approval) (json.RawMessage, error)) string {
	t.Helper()
	turn := agent.RunningTurn{Ctx: context.Background(), TurnID: "t1", ChatID: "claude:s1", Cwd: cwd, Device: "phone", Config: cfg, Ask: ask}
	entry, release := s.Attach(&fakeSession{turn: turn}, agent.TurnRequest{Cwd: cwd})
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

// A host without a runtime (a dev build without the SDK) offers neither tool,
// and refuses a call by name.
func TestPluginsToolsNeedTheSDK(t *testing.T) {
	s := pluginsServer(t, pluginsService(t, ""))
	token := attachIn(t, s, t.TempDir())
	if got := toolNames(t, s, token); got != "browser" {
		t.Fatalf("tools without the SDK: %q", got)
	}
	if got := callTool(t, s, token, "plugins", map[string]any{}); !got.IsError || !strings.Contains(got.Content[0].Text, "unknown tool") {
		t.Fatalf("call without the SDK: %+v", got)
	}
}

// With the runtime, every project gets them, so an agent can build the first
// plugin; an environment without plugins says so.
func TestPluginsToolsComeWithTheRuntime(t *testing.T) {
	s := pluginsServer(t, pluginsService(t, "/sdk"))
	token := attachIn(t, s, t.TempDir())
	if got := toolNames(t, s, token); got != "browser,plugins,plugin_call" {
		t.Fatalf("tools with the runtime: %q", got)
	}
	if got := callTool(t, s, token, "plugins", map[string]any{}); !got.IsError || !strings.Contains(got.Content[0].Text, "this environment has no plugins") {
		t.Fatalf("plugins in a project without them: %+v", got)
	}
}

// A failed call keeps the plugin's code and field errors for the agent to act on.
func TestPluginResultKeepsThePluginsErrorShape(t *testing.T) {
	got := pluginResult(outcome.Answer{Error: &outcome.Failure{Code: "VALIDATION", Message: "Check the highlighted fields.", FieldErrors: map[string]string{"/status": "Invalid option"}}}, nil)
	if !got.IsError || !strings.Contains(got.Content[0].Text, `"/status"`) {
		t.Fatalf("%+v", got)
	}
	got = pluginResult(outcome.Answer{Result: json.RawMessage(`{"id":"p1"}`)}, nil)
	if got.IsError || !strings.Contains(got.Content[0].Text, `"p1"`) {
		t.Fatalf("%+v", got)
	}
}

// End to end through the plugins server and this repo's SDK: an environment
// plugin, called from a project.
func TestPluginCallRunsAnEnvironmentPlugin(t *testing.T) {
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
	root := t.TempDir()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "@repogo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sdk, filepath.Join(dir, "node_modules", "@repogo", "plugins")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	plugin := `import { definePlugin, method, z } from "@repogo/plugins";
export default definePlugin({
  namespace: "notes",
  collections: { notes: z.object({ text: z.string() }) },
  methods: {
    tidy: method(z.object({}), () => "tidied", { approve: "Tidy the notes?", approveUpTo: "auto-accept-edits" }),
  },
});
`
	if err := os.WriteFile(filepath.Join(dir, "notes", "app.ts"), []byte(plugin), 0o644); err != nil {
		t.Fatal(err)
	}
	s := pluginsServer(t, pluginsServiceIn(t, sdk, dir))
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

	// A delete asks first, from an agent as from a tap; the record stays until the user's yes.
	var note struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(created.Content[0].Text), &note); err != nil || note.ID == "" {
		t.Fatalf("created note: %q %v", created.Content[0].Text, err)
	}
	asked := callTool(t, s, token, "plugin_call", map[string]any{"method": "notes.notes.delete", "input": map[string]any{"id": note.ID}})
	if !asked.IsError || !strings.Contains(asked.Content[0].Text, "CONFIRM") {
		t.Fatalf("delete without confirmed: %+v", asked)
	}
	if kept := callTool(t, s, token, "plugin_call", map[string]any{"method": "notes.notes.get", "input": map[string]any{"id": note.ID}}); kept.IsError || !strings.Contains(kept.Content[0].Text, `"hi"`) {
		t.Fatalf("the note went before the user agreed: %+v", kept)
	}
	if !strings.Contains(asked.Content[0].Text, `action="plugin:notes.notes.delete"`) {
		t.Fatalf("the agent isn't told to draw a card: %+v", asked)
	}
	// The agent can't answer the question itself: confirmed from it is ignored.
	if self := callTool(t, s, token, "plugin_call", map[string]any{"method": "notes.notes.delete", "input": map[string]any{"id": note.ID}, "confirmed": true}); !self.IsError || !strings.Contains(self.Content[0].Text, "CONFIRM") {
		t.Fatalf("an agent confirmed its own delete: %+v", self)
	}
	if kept := callTool(t, s, token, "plugin_call", map[string]any{"method": "notes.notes.get", "input": map[string]any{"id": note.ID}}); kept.IsError || !strings.Contains(kept.Content[0].Text, `"hi"`) {
		t.Fatalf("the note went on the agent's own yes: %+v", kept)
	}

	// In a chat that can ask, the question goes to the user as an approval:
	// their no keeps the note, their yes deletes it.
	var approvals []agent.Approval
	pick := approveNoID
	answer := func(_ context.Context, a agent.Approval) (json.RawMessage, error) {
		approvals = append(approvals, a)
		return json.Marshal(pick)
	}
	chat := attachAsking(t, s, root, agent.TurnConfig{PermissionMode: agent.PermissionFullAccess}, func(_ context.Context, a agent.Approval) (json.RawMessage, error) {
		approvals = append(approvals, a)
		return json.Marshal(pick)
	})
	declined := callTool(t, s, chat, "plugin_call", map[string]any{"method": "notes.notes.delete", "input": map[string]any{"id": note.ID}})
	if !declined.IsError || !strings.Contains(declined.Content[0].Text, "declined") {
		t.Fatalf("a no: %+v", declined)
	}
	if len(approvals) != 1 || approvals[0].Title == "" || len(approvals[0].Options) != 2 || !strings.Contains(string(approvals[0].Input), "notes.notes.delete") {
		t.Fatalf("the approval: %+v", approvals)
	}
	if kept := callTool(t, s, chat, "plugin_call", map[string]any{"method": "notes.notes.get", "input": map[string]any{"id": note.ID}}); kept.IsError {
		t.Fatalf("the note went on the user's no: %+v", kept)
	}
	pick = approveYesID
	if done := callTool(t, s, chat, "plugin_call", map[string]any{"method": "notes.notes.delete", "input": map[string]any{"id": note.ID}}); done.IsError {
		t.Fatalf("a yes: %+v", done)
	}
	if gone := callTool(t, s, chat, "plugin_call", map[string]any{"method": "notes.notes.get", "input": map[string]any{"id": note.ID}}); gone.IsError || strings.Contains(gone.Content[0].Text, `"hi"`) {
		t.Fatalf("the note stayed after the user's yes: %+v", gone)
	}

	// approveUpTo auto-accept-edits: full access runs it unasked, the stricter modes ask.
	approvals, pick = nil, approveNoID
	if ran := callTool(t, s, chat, "plugin_call", map[string]any{"method": "notes.tidy"}); ran.IsError || !strings.Contains(ran.Content[0].Text, "tidied") || len(approvals) != 0 {
		t.Fatalf("full access past approveUpTo: %+v, asked %d", ran, len(approvals))
	}
	for _, mode := range []agent.PermissionMode{agent.PermissionAutoAcceptEdits, agent.PermissionApprovalRequired, ""} {
		approvals = nil
		strict := attachAsking(t, s, root, agent.TurnConfig{PermissionMode: mode}, answer)
		if got := callTool(t, s, strict, "plugin_call", map[string]any{"method": "notes.tidy"}); !got.IsError || len(approvals) != 1 || approvals[0].Title != "Tidy the notes?" {
			t.Fatalf("%q within approveUpTo: %+v, asked %d", mode, got, len(approvals))
		}
	}

	// Plan mode runs reads only, whatever the method asks.
	planning := attachAsking(t, s, root, agent.TurnConfig{PermissionMode: agent.PermissionFullAccess, Mode: "plan"}, answer)
	for _, method := range []string{"notes.tidy", "notes.notes.create"} {
		if got := callTool(t, s, planning, "plugin_call", map[string]any{"method": method, "input": map[string]any{"text": "plan"}}); !got.IsError || !strings.Contains(got.Content[0].Text, "plan mode") {
			t.Fatalf("%s in plan mode: %+v", method, got)
		}
	}
	if listed := callTool(t, s, planning, "plugin_call", map[string]any{"method": "notes.notes.list"}); listed.IsError {
		t.Fatalf("a read in plan mode: %+v", listed)
	}
}

func TestAsksIn(t *testing.T) {
	for _, c := range []struct {
		upTo string
		mode agent.PermissionMode
		want bool
	}{
		{"", agent.PermissionFullAccess, true},
		{"full-access", agent.PermissionFullAccess, true},
		{"auto-accept-edits", agent.PermissionFullAccess, false},
		{"auto-accept-edits", agent.PermissionAutoAcceptEdits, true},
		{"approval-required", agent.PermissionAutoAcceptEdits, false},
		{"approval-required", agent.PermissionApprovalRequired, true},
		{"approval-required", "", true},
		{"approval-required", "yolo", true},
	} {
		if got := asksIn(c.upTo, c.mode); got != c.want {
			t.Errorf("asksIn(%q, %q) = %v", c.upTo, c.mode, got)
		}
	}
}
