package codexappserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("REPOGO_FAKE_APP_SERVER") == "1" {
		fakeServer()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeServer refuses "refuse", and on "ask" sends a request of its own, then
// settles it itself before anyone answers.
func fakeServer() {
	out := json.NewEncoder(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var f struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if json.Unmarshal(in.Bytes(), &f) != nil {
			os.Exit(12)
		}
		switch f.Method {
		case "refuse":
			_ = out.Encode(map[string]any{"id": f.ID, "error": map[string]any{"code": -32600, "message": "not allowed"}})
		case "ask":
			_ = out.Encode(map[string]any{"id": "req-1", "method": MethodUserInput, "params": map[string]any{"threadId": "t"}})
			_ = out.Encode(map[string]any{"method": MethodServerRequestResolved, "params": map[string]any{"threadId": "t", "requestId": "req-1"}})
			_ = out.Encode(map[string]any{"id": f.ID, "result": map[string]string{"ok": "yes"}})
		}
	}
}

func start(t *testing.T, h Handlers) *Client {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Start(t.Context(), Options{Executable: executable, Cwd: t.TempDir(),
		Env: append(os.Environ(), "REPOGO_FAKE_APP_SERVER=1")}, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	go func() {
		for range c.Notifications() {
		}
	}()
	return c
}

func TestRefusedCallIsAnError(t *testing.T) {
	c := start(t, Handlers{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var refused *Error
	if err := c.Call(ctx, "refuse", nil, nil); !errors.As(err, &refused) || refused.Message != "not allowed" {
		t.Fatalf("got %v", err)
	}
}

func TestResolvedRequestIsWithdrawn(t *testing.T) {
	withdrawn := make(chan struct{})
	c := start(t, Handlers{Request: func(ctx context.Context, r Request) (any, error) {
		if r.Method != MethodUserInput || string(r.ID) != `"req-1"` {
			t.Errorf("request %s %s", r.ID, r.Method)
		}
		<-ctx.Done()
		close(withdrawn)
		return nil, ctx.Err()
	}})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var out struct {
		OK string `json:"ok"`
	}
	if err := c.Call(ctx, "ask", nil, &out); err != nil || out.OK != "yes" {
		t.Fatalf("%v %#v", err, out)
	}
	select {
	case <-withdrawn:
	case <-ctx.Done():
		t.Fatal("a request Codex resolved kept waiting on a person")
	}
}

func TestProcessExitEndsPendingCalls(t *testing.T) {
	c := start(t, Handlers{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() { _ = c.Close() }()
	if err := c.Call(ctx, "never-answered", nil, nil); err == nil {
		t.Fatal("a call outlived its process")
	}
}
