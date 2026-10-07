package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv("REPOGO_FAKE_CLAUDE"); mode != "" {
		fakeClaude(mode)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeClaude(mode string) {
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	type request struct {
		Type      string          `json:"type"`
		RequestID string          `json:"request_id"`
		Request   map[string]any  `json:"request"`
		Response  controlResponse `json:"response"`
	}
	var held []request
	respond := func(r request, value any) {
		_ = encoder.Encode(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": r.RequestID, "response": value}})
	}
	for scanner.Scan() {
		var r request
		if json.Unmarshal(scanner.Bytes(), &r) != nil {
			os.Exit(10)
		}
		if r.Type == "control_response" {
			_ = encoder.Encode(map[string]any{"type": "system", "subtype": "permission_answer", "result": string(r.Response.Response)})
			continue
		}
		switch mode {
		case "exit":
			fmt.Fprint(os.Stderr, strings.Repeat("x", maxStderr+300)+"exit detail")
			os.Exit(23)
		case "malformed":
			fmt.Fprintln(os.Stdout, "invalid-json")
			continue
		case "out-of-order":
			held = append(held, r)
			if len(held) == 2 {
				for i := 1; i >= 0; i-- {
					respond(held[i], map[string]any{"echo": held[i].Request["value"]})
				}
				held = nil
			}
		case "permission":
			if r.Request["subtype"] == "initialize" {
				_ = encoder.Encode(map[string]any{"type": "control_request", "request_id": "permission-1", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{"command": "pwd"}, "tool_use_id": "tool-1"}})
			}
			respond(r, map[string]any{})
		case "cancel":
			_ = encoder.Encode(map[string]any{"type": "control_request", "request_id": "permission-1", "request": map[string]any{"subtype": "can_use_tool", "tool_name": "Bash", "input": map[string]any{}, "tool_use_id": "tool-1"}})
			_ = encoder.Encode(map[string]any{"type": "control_cancel_request", "request_id": "permission-1"})
			respond(r, map[string]any{})
		default:
			respond(r, map[string]any{})
		}
	}
}

func testClient(t *testing.T, mode string, handlers Handlers) *Client {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Start(t.Context(), Options{Executable: binary, Cwd: t.TempDir(), Env: append(os.Environ(), "REPOGO_FAKE_CLAUDE="+mode)}, handlers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func boundedContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestControlMatchesOutOfOrder(t *testing.T) {
	c := testClient(t, "out-of-order", Handlers{})
	ctx := boundedContext(t)
	var group sync.WaitGroup
	for _, value := range []string{"first", "second"} {
		group.Add(1)
		go func() {
			defer group.Done()
			var response struct {
				Echo string `json:"echo"`
			}
			err := c.requestControl(ctx, map[string]string{"subtype": "echo", "value": value}, &response)
			if err != nil || response.Echo != value {
				t.Errorf("response %q, error %v; want %q", response.Echo, err, value)
			}
		}()
	}
	group.Wait()
}

func TestPermissionDoesNotBlockControlReader(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan PermissionRequest, 1)
	c := testClient(t, "permission", Handlers{CanUseTool: func(ctx context.Context, p PermissionRequest) (PermissionResult, error) {
		entered <- p
		select {
		case <-release:
			return PermissionResult{Behavior: "allow", UpdatedInput: p.Input, ToolUseID: p.ToolUseID}, nil
		case <-ctx.Done():
			return PermissionResult{}, ctx.Err()
		}
	}})
	ctx := boundedContext(t)
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-entered:
		if p.ToolUseID != "tool-1" {
			t.Fatal(p)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := c.SetModel(ctx, "sonnet"); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case m := <-c.Messages():
		if !strings.Contains(m.Result, `"behavior":"allow"`) {
			t.Fatal(m.Result)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestInboundCancellation(t *testing.T) {
	cancelled := make(chan struct{})
	c := testClient(t, "cancel", Handlers{CanUseTool: func(ctx context.Context, _ PermissionRequest) (PermissionResult, error) {
		<-ctx.Done()
		close(cancelled)
		return PermissionResult{}, ctx.Err()
	}})
	ctx := boundedContext(t)
	if _, err := c.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestExitRetainsCodeAndBoundedStderr(t *testing.T) {
	c := testClient(t, "exit", Handlers{})
	ctx := boundedContext(t)
	if _, err := c.Initialize(ctx); err == nil {
		t.Fatal("expected exit error")
	}
	select {
	case <-c.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	exit := c.Exit()
	if exit.Code != 23 || len(exit.Stderr) != maxStderr || !strings.HasSuffix(exit.Stderr, "exit detail") {
		t.Fatalf("exit code %d, stderr length %d", exit.Code, len(exit.Stderr))
	}
}

func TestMalformedFrameFailsProcess(t *testing.T) {
	c := testClient(t, "malformed", Handlers{})
	ctx := boundedContext(t)
	_, _ = c.Initialize(ctx)
	select {
	case <-c.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := c.Exit().Err; err == nil || !strings.Contains(err.Error(), "malformed stdout") {
		t.Fatal(err)
	}
}

func TestLaunchOptions(t *testing.T) {
	args, err := (Options{Executable: "claude", Cwd: "/tmp", ResumeID: "session", SettingSources: []string{"user", "project", "local"}, MCPServers: map[string]MCPServer{"test": {Type: "http", URL: "http://localhost/mcp"}}}).args()
	if err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"--include-partial-messages", "--permission-prompt-tool", "stdio", "--resume", "session", "--setting-sources=user,project,local", "--mcp-config"} {
		if !slices.Contains(args, flag) {
			t.Errorf("missing %s", flag)
		}
	}
	if _, err = (Options{Executable: "claude", Cwd: "/tmp", ResumeID: "a", SessionID: "b"}).args(); err == nil {
		t.Fatal("accepted both session and resume")
	}
}

func TestMessageContentVariants(t *testing.T) {
	for _, raw := range []string{`{"type":"user","message":{"content":"hello"}}`, `{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`} {
		var m Message
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		if len(m.Message.Content) != 1 || m.Message.Content[0].Text != "hello" {
			t.Fatal(m.Message)
		}
	}
}
