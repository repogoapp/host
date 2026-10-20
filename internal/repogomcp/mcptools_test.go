package repogomcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/mcp"
)

// mcpService is an MCP service with no servers connected.
func mcpService(t *testing.T) *mcp.Service {
	t.Helper()
	s, err := mcp.Open(filepath.Join(t.TempDir(), "mcp.json"), func(mcp.Changed) {})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// mcpServiceFunc is mcpService as the server reads it.
func mcpServiceFunc(t *testing.T) func() *mcp.Service {
	s := mcpService(t)
	return func() *mcp.Service { return s }
}

// docsServer is an MCP server needing no sign-in, with a read and a write.
func docsServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&msg)
		result := `{}`
		switch msg.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			result = `{"tools":[{"name":"search","annotations":{"readOnlyHint":true}},{"name":"publish"}]}`
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":%s}`, msg.ID, result)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mcp_tools comes only to a project with a server switched on, and lists that
// server under the key a card names it with.
func TestMCPToolsListsTheProjectsServers(t *testing.T) {
	service := mcpService(t)
	project, other := t.TempDir(), t.TempDir()
	probe, err := service.Probe(t.Context(), docsServer(t).URL, "Team Docs", project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Connect(probe.ProbeID, ""); err != nil {
		t.Fatal(err)
	}
	s := New(Config{
		URL: func() string { return "http://127.0.0.1:1/mcp/repogo" }, On: func(string) bool { return true },
		Browser: newFakePhone(true, nil).browser, Plugins: pluginsService(t, ""), MCP: func() *mcp.Service { return service },
	})

	if got := toolNames(t, s, attachIn(t, s, other)); got != "browser" {
		t.Fatalf("a project without servers got %q", got)
	}
	token := attachIn(t, s, project)
	if got := toolNames(t, s, token); got != "browser,mcp_tools" {
		t.Fatalf("a project with a server got %q", got)
	}
	got := callTool(t, s, token, "mcp_tools", map[string]any{})
	if got.IsError {
		t.Fatalf("mcp_tools failed: %+v", got)
	}
	var listing struct {
		Servers []mcp.ServerTools `json:"servers"`
	}
	if err := json.Unmarshal([]byte(got.Content[0].Text), &listing); err != nil {
		t.Fatalf("%v: %s", err, got.Content[0].Text)
	}
	var lines []string
	for _, server := range listing.Servers {
		for _, tool := range server.Tools {
			lines = append(lines, fmt.Sprintf("%s (%s) %s read_only=%v asks_first=%v", server.Server, server.Label, tool.Name, tool.ReadOnly, tool.AsksFirst))
		}
	}
	want := "team_docs (Team Docs) publish read_only=false asks_first=true, team_docs (Team Docs) search read_only=true asks_first=false"
	if strings.Join(lines, ", ") != want {
		t.Fatalf("servers = %v, want %s", lines, want)
	}
	// Cards need names and flags; a voice catalog is what describes each tool.
	if text := got.Content[0].Text; strings.Contains(text, `"description"`) || strings.Contains(text, `"input"`) {
		t.Fatalf("mcp_tools described its tools: %s", text)
	}
	if !strings.Contains(s.instructions(project), "mcp_tools") || strings.Contains(s.instructions(other), "mcp_tools") {
		t.Fatal("the instructions name mcp_tools where it isn't offered")
	}
}
