package wsconn

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/internal/handshake"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/wirecodec"
)

// page is a message large and repetitive enough that deflate always pays.
var page = `{"jsonrpc":"2.0","method":"page","params":{"text":"` + strings.Repeat("retry the upload ", 400) + `"}}`

// handshaken runs a handshake with askFor as the hello's flag; the server then
// sends page and echoes the client's next message, reporting its frame type.
// It returns the client's Conn, the raw socket beneath it, and that type.
func handshaken(t *testing.T, askFor bool) (*Conn, *websocket.Conn, <-chan websocket.MessageType) {
	t.Helper()
	got := make(chan websocket.MessageType, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c := Wrap(ws)
		ctx := r.Context()
		if _, err := c.Accept(ctx, "test", "dev", func(handshake.Hello, []byte) error { return nil }); err != nil {
			return
		}
		if err := c.Write(ctx, websocket.MessageText, []byte(page)); err != nil {
			return
		}
		typ, frame, err := ws.Read(ctx)
		if err != nil {
			return
		}
		got <- typ
		if typ == websocket.MessageBinary {
			if frame, err = wirecodec.Decode(frame); err != nil {
				return
			}
		}
		c.Write(ctx, websocket.MessageText, frame)
		c.Read(ctx)
	}))
	t.Cleanup(srv.Close)
	ws, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	c := Wrap(ws)
	if _, err := c.Hello(context.Background(), handshake.Hello{DeviceID: "d", Role: handshake.RoleClient, Deflate: askFor}, nil); err != nil {
		t.Fatal(err)
	}
	return c, ws, got
}

// A hello that asks for deflate gets it: the host's page arrives as a binary
// frame that deflated, and the phone's message goes back the same way.
func TestAgreedDeflateCompressesBothWays(t *testing.T) {
	c, ws, got := handshaken(t, true)
	if !c.Deflating() {
		t.Fatal("the hello asked for deflate and the reply didn't confirm it")
	}
	typ, frame, err := ws.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageBinary || frame[0] != wirecodec.Deflate || len(frame)*4 > len(page) {
		t.Fatalf("page came as type %v, codec %#x, %d bytes for %d", typ, frame[0], len(frame), len(page))
	}
	if err := c.SendJSON(context.Background(), mustDecode(t, page)); err != nil {
		t.Fatal(err)
	}
	if typ := <-got; typ != websocket.MessageBinary {
		t.Fatalf("the phone's message went as %v, want a binary frame", typ)
	}
	_, echo, err := c.Read(context.Background())
	if err != nil || len(echo) < len(page)-64 || !strings.Contains(string(echo), "retry the upload") {
		t.Fatalf("the echo of a compressed message came back as %d bytes, %v", len(echo), err)
	}
}

// Without the flag, as from an app that predates it, frames stay plain text.
func TestAHelloWithoutDeflateStaysPlain(t *testing.T) {
	c, ws, _ := handshaken(t, false)
	if c.Deflating() {
		t.Fatal("deflate was on though the hello didn't ask")
	}
	typ, frame, err := ws.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageText || string(frame) != page {
		t.Fatalf("page came as type %v, %d bytes; want the plain text", typ, len(frame))
	}
}

func mustDecode(t *testing.T, s string) *jsonrpc.Message {
	t.Helper()
	m, err := jsonrpc.Decode([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return m
}
