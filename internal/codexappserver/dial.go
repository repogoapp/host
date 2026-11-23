package codexappserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/coder/websocket"
	"github.com/repogoapp/host/internal/stdiorpc"
)

// maxMessage is the largest message read from the background server, the
// same cap stdio's line reader has.
const maxMessage = 16 << 20

const dialTimeout = 5 * time.Second

// SocketPath is where Codex's background server listens, under its home.
// Codex starts it for the terminal and the desktop app (0.169 and later), and
// every client of it shares its threads live.
func SocketPath(home string) string {
	return filepath.Join(home, "app-server-control", "app-server-control.sock")
}

// Dial connects to the background server at socket. It speaks WebSocket
// over the Unix socket, one JSON-RPC message per text frame; the socket's
// file permissions are its only lock. The client lives until ctx ends, the
// server hangs up or it is closed; closing it leaves the server and its
// threads running.
func Dial(ctx context.Context, socket string, handlers Handlers) (*Client, error) {
	dial, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socket)
	}}
	ws, _, err := websocket.Dial(dial, "ws://localhost/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(maxMessage)
	lines := newFrameLines(ctx, ws)
	return &Client{Client: stdiorpc.Connect(ctx, lines, "", handlers.rpc())}, nil
}

// frameLines turns WebSocket text frames into the lines stdiorpc reads and
// writes: each frame read is one line, and each line written one frame.
type frameLines struct {
	ws     *websocket.Conn
	ctx    context.Context
	cancel context.CancelFunc
	buf    []byte
}

func newFrameLines(ctx context.Context, ws *websocket.Conn) *frameLines {
	ctx, cancel := context.WithCancel(ctx)
	return &frameLines{ws: ws, ctx: ctx, cancel: cancel}
}

func (f *frameLines) Read(p []byte) (int, error) {
	for len(f.buf) == 0 {
		kind, data, err := f.ws.Read(f.ctx)
		if err != nil {
			if websocket.CloseStatus(err) != -1 {
				return 0, io.EOF
			}
			return 0, err
		}
		if kind != websocket.MessageText {
			continue
		}
		// A message is one line however the server spaced it.
		if bytes.IndexByte(data, '\n') >= 0 {
			var compact bytes.Buffer
			if json.Compact(&compact, data) == nil {
				data = compact.Bytes()
			}
		}
		f.buf = append(data, '\n')
	}
	n := copy(p, f.buf)
	f.buf = f.buf[n:]
	return n, nil
}

// Write sends one line, which stdiorpc writes whole, as one text frame.
func (f *frameLines) Write(p []byte) (int, error) {
	if err := f.ws.Write(f.ctx, websocket.MessageText, bytes.TrimSuffix(p, []byte("\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (f *frameLines) Close() error {
	f.cancel()
	return f.ws.CloseNow()
}
