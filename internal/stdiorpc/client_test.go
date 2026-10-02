package stdiorpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("REPOGO_FAKE_STDIORPC") == "1" {
		fakeRPC()
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func fakeRPC() {
	out := json.NewEncoder(os.Stdout)
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var frame struct {
			Version string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Error   *Error          `json:"error"`
		}
		if json.Unmarshal(in.Bytes(), &frame) != nil || frame.Version != "2.0" {
			os.Exit(20)
		}
		switch frame.Method {
		case "ordered":
			for _, text := range []string{"one", "two"} {
				_ = out.Encode(map[string]any{"jsonrpc": "2.0", "method": "update", "params": text})
			}
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": frame.ID, "result": "done"})
		case "ask":
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": "server-id", "method": "unknown", "params": struct{}{}})
		case "malformed":
			_, _ = os.Stdout.WriteString("not json\n")
		case "exit":
			os.Exit(17)
		case "wait":
		case "":
			if string(frame.ID) != `"server-id"` || frame.Error == nil || frame.Error.Code != -32601 {
				os.Exit(21)
			}
			_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": "rejected"})
		}
	}
}
func startFake(t *testing.T, h Handlers) *Client {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := Start(t.Context(), Options{Executable: path, Cwd: t.TempDir(), Env: append(os.Environ(), "REPOGO_FAKE_STDIORPC=1"), Version: "2.0"}, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
func TestNotificationsPrecedePromptResponse(t *testing.T) {
	var mu sync.Mutex
	var values []string
	c := startFake(t, Handlers{Notification: func(n Notification) error {
		var s string
		if err := json.Unmarshal(n.Params, &s); err != nil {
			return err
		}
		mu.Lock()
		values = append(values, s)
		mu.Unlock()
		return nil
	}})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var result string
	if err := c.Call(ctx, "ordered", nil, &result); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if result != "done" || strings.Join(values, ",") != "one,two" {
		t.Fatalf("%q %v", result, values)
	}
}
func TestUnknownRequestReturnsTypedErrorAndPreservesStringID(t *testing.T) {
	c := startFake(t, Handlers{})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var result string
	if err := c.Call(ctx, "ask", nil, &result); err != nil || result != "rejected" {
		t.Fatalf("%q %v", result, err)
	}
}
func TestMalformedFrameAndExitEndPendingCalls(t *testing.T) {
	for _, method := range []string{"malformed", "exit"} {
		t.Run(method, func(t *testing.T) {
			c := startFake(t, Handlers{})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := c.Call(ctx, method, nil, nil)
			if err == nil || errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%v", err)
			}
			<-c.Done()
			if method == "exit" && c.Exit().Code != 17 {
				t.Fatalf("exit %+v", c.Exit())
			}
		})
	}
}
func TestCloseEndsPendingCalls(t *testing.T) {
	c := startFake(t, Handlers{})
	done := make(chan error, 1)
	go func() { done <- c.Call(t.Context(), "wait", nil, nil) }()
	_ = c.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed process succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pending call hung")
	}
}
