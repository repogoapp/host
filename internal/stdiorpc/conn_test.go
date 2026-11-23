package stdiorpc

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"
)

// A connected client calls, receives notifications, and ends when the
// server hangs up, without owning a process.
func TestConnectCallsAndEndsWithTheConnection(t *testing.T) {
	ours, theirs := net.Pipe()
	go func() {
		in := bufio.NewScanner(theirs)
		out := json.NewEncoder(theirs)
		for in.Scan() {
			var frame struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			_ = json.Unmarshal(in.Bytes(), &frame)
			_ = out.Encode(map[string]any{"method": "update", "params": "one"})
			_ = out.Encode(map[string]any{"id": frame.ID, "result": frame.Method})
			_ = theirs.Close()
			return
		}
	}()
	c := Connect(context.Background(), ours, "", Handlers{})
	var got string
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Call(ctx, "hello", nil, &got); err != nil || got != "hello" {
		t.Fatalf("call = %q, %v", got, err)
	}
	if n := <-c.Notifications(); n.Method != "update" {
		t.Fatalf("notification = %+v", n)
	}
	select {
	case <-c.Done():
	case <-ctx.Done():
		t.Fatal("client outlived its connection")
	}
	if c.Exit().Err == nil {
		t.Fatal("a server hanging up should be the client's exit error")
	}
}

// Closing a connected client closes the connection and ends the client.
func TestConnectCloseClosesTheConnection(t *testing.T) {
	ours, theirs := net.Pipe()
	c := Connect(context.Background(), ours, "", Handlers{})
	read := make(chan error, 1)
	go func() { _, err := theirs.Read(make([]byte, 1)); read <- err }()
	_ = c.Close()
	select {
	case err := <-read:
		if err == nil {
			t.Fatal("connection still open after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not close the connection")
	}
}
