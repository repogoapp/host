package codexappserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeBackgroundServer answers each call with its method, after a
// notification sent pretty-printed, as one frame across several lines.
func fakeBackgroundServer(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		for {
			_, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			var f struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
			}
			_ = json.Unmarshal(data, &f)
			if len(f.ID) == 0 {
				continue
			}
			note, _ := json.MarshalIndent(map[string]any{"method": "thread/started", "params": map[string]string{"threadId": "t"}}, "", "  ")
			_ = ws.Write(r.Context(), websocket.MessageText, note)
			reply, _ := json.Marshal(map[string]any{"id": f.ID, "result": f.Method})
			_ = ws.Write(r.Context(), websocket.MessageText, reply)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return socket
}

func TestDialSpeaksToTheBackgroundServer(t *testing.T) {
	socket := fakeBackgroundServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := Dial(context.Background(), socket, Handlers{})
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := client.Call(ctx, "thread/list", nil, &got); err != nil || got != "thread/list" {
		t.Fatalf("call = %q, %v", got, err)
	}
	if n := <-client.Notifications(); n.Method != "thread/started" {
		t.Fatalf("notification = %+v", n)
	}
	_ = client.Close()
	select {
	case <-client.Done():
	case <-ctx.Done():
		t.Fatal("client did not end on Close")
	}
}

func TestDialFailsWithoutAServer(t *testing.T) {
	if _, err := Dial(context.Background(), filepath.Join(t.TempDir(), "none.sock"), Handlers{}); err == nil {
		t.Fatal("dialed a socket that does not exist")
	}
}
