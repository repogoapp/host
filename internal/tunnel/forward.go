package tunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/repogo/host/internal/forward"
	gw "github.com/repogo/host/internal/tunnel/wire/gateway"
	pv "github.com/repogo/host/internal/tunnel/wire/preview"
)

// Every forwarded request is checked against the allowlist and dialled on
// 127.0.0.1 only.

const (
	bufferedBodyLimit   = 1 << 20
	streamingChunkSize  = 512 << 10
	sseChunkSize        = 4 << 10
	streamFlushInterval = 20 * time.Millisecond
	wsReadLimit         = 32 << 20
)

// Dropped in both directions along with forward.HopByHop. Accept-Encoding
// too: the dev server then sends plain bytes, which the gateway compresses.
var gatewayOwned = map[string]bool{
	"host": true, "content-encoding": true, "accept-encoding": true,
	"x-forwarded-proto": true, "x-forwarded-host": true, "x-forwarded-for": true,
	"x-forwarded-port": true, "x-forwarded-ssl": true, "forwarded": true,
}

// dropped reports a header that never crosses the tunnel.
func dropped(name string) bool {
	return forward.HopByHop(name) || gatewayOwned[strings.ToLower(name)]
}

var wsHandshake = map[string]bool{
	"sec-websocket-key": true, "sec-websocket-version": true,
	"sec-websocket-protocol": true, "sec-websocket-extensions": true,
}

// Recognized assets stream even when their declared length is small.
var finiteAssets = map[string]bool{
	"avif": true, "cjs": true, "css": true, "gif": true, "ico": true,
	"jpeg": true, "jpg": true, "js": true, "json": true, "jsx": true,
	"map": true, "mjs": true, "mp4": true, "otf": true, "png": true,
	"svg": true, "ttf": true, "wasm": true, "webm": true, "webp": true,
	"woff": true, "woff2": true,
}

// loopback is the only way a tunnel reaches a socket; no timeout, since
// responses may stream for as long as the visitor stays.
var loopback = forward.Loopback(0)

func headerValue(lines []string, name string) string {
	for _, line := range lines {
		if k, v, ok := strings.Cut(line, ": "); ok && strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// resolve checks a gateway request against the allowlist: the Host line the
// gateway adds names an open slug, and the row's target is that slug's port.
// It returns the loopback base URL and the public host.
func (l *link) resolve(target string, lines []string) (base, public string, ok bool) {
	public = strings.ToLower(headerValue(lines, "host"))
	slug, domain, _ := strings.Cut(public, ".")
	if domain != Domain {
		return "", "", false
	}
	port, open := l.s.allowed(slug)
	if !open || target != strconv.Itoa(port) {
		return "", "", false
	}
	return "http://127.0.0.1:" + target, public, true
}

func requestPath(p string) (string, bool) {
	if p == "" {
		return "/", true
	}
	return p, strings.HasPrefix(p, "/")
}

func copyHeaders(dst http.Header, lines []string, skip map[string]bool) {
	for _, line := range lines {
		k, v, ok := strings.Cut(line, ": ")
		lk := strings.ToLower(k)
		if !ok || k == "" || dropped(k) || skip[lk] {
			continue
		}
		dst.Add(k, v)
	}
}

// The public request reached the gateway over HTTPS; without these the app
// builds absolute links against 127.0.0.1.
func forwarded(h http.Header, public string) {
	h.Set("X-Forwarded-Proto", "https")
	h.Set("X-Forwarded-Host", public)
}

func (l *link) serveHTTP(ctx context.Context, req *pv.PreviewRequest) {
	id := req.GetSessionId()
	if id == "" {
		return
	}
	base, public, ok := l.resolve(req.GetTarget(), req.GetHeaderLines())
	if !ok {
		l.s.cfg.Log.Warn("tunnel: refused a request for a tunnel this host did not open", "target", req.GetTarget())
		l.sendError(id, http.StatusForbidden, "This computer isn't serving this tunnel.", "Tunnel closed")
		return
	}
	path, ok := requestPath(req.GetPath())
	if !ok {
		l.sendError(id, http.StatusBadRequest, "The request path is invalid.", "Bad request")
		return
	}
	method := strings.ToUpper(req.GetMethod())
	if method == "" {
		method = http.MethodGet
	}
	var body io.Reader
	if len(req.GetBody()) > 0 {
		body = bytes.NewReader(req.GetBody())
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		l.sendError(id, http.StatusBadGateway, "Invalid request: "+err.Error(), "Device error")
		return
	}
	copyHeaders(httpReq.Header, req.GetHeaderLines(), nil)
	forwarded(httpReq.Header, public)

	resp, err := loopback.Do(httpReq)
	if err != nil {
		l.sendError(id, http.StatusBadGateway,
			"Couldn't reach the local server on port "+req.GetTarget()+". Make sure it's running and listening.",
			"Local server unreachable")
		return
	}
	defer resp.Body.Close()
	lines := make([]string, 0, len(resp.Header))
	for k, vs := range resp.Header {
		if dropped(k) {
			continue
		}
		for _, v := range vs {
			lines = append(lines, k+": "+v)
		}
	}
	status := uint32(resp.StatusCode)
	empty := method == http.MethodHead || resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified
	if !empty && (shouldStream(resp) || isFiniteAsset(req, method)) {
		l.streamBody(id, status, lines, resp)
		return
	}
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		l.sendError(id, http.StatusBadGateway, "The local server closed the connection before the response completed.", "Local server disconnected")
		return
	}
	_ = l.sendResponse(id, &pv.PreviewResponse{Status: status, HeaderLines: lines, Body: buf})
}

// streamBody reads on its own goroutine so a paused upstream cannot hold back a
// partial flush.
func (l *link) streamBody(id string, status uint32, lines []string, resp *http.Response) {
	if err := l.sendResponse(id, &pv.PreviewResponse{Status: status, HeaderLines: lines, Streaming: true}); err != nil {
		return
	}
	type read struct {
		data []byte
		err  error
	}
	reads := make(chan read)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			chunk := make([]byte, 32<<10)
			n, err := resp.Body.Read(chunk)
			select {
			case reads <- read{chunk[:n], err}:
			case <-done:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	sse := isSSE(resp)
	flushAt := streamingChunkSize
	if sse {
		flushAt = sseChunkSize
	}
	tick := time.NewTicker(streamFlushInterval)
	defer tick.Stop()
	pending := make([]byte, 0, flushAt)
	flush := func(final bool) error {
		err := l.send(&gw.ClientMessage{Body: &gw.ClientMessage_HttpBodyChunk{
			HttpBodyChunk: &pv.PreviewBodyChunk{SessionId: id, Data: pending, EndOfStream: final},
		}})
		pending = make([]byte, 0, flushAt)
		return err
	}
	for {
		select {
		case r := <-reads:
			pending = append(pending, r.data...)
			boundary := sse && bytes.HasSuffix(pending, []byte("\n\n"))
			if len(pending) >= flushAt || boundary {
				if flush(false) != nil {
					return
				}
			}
			if r.err != nil {
				_ = flush(true)
				return
			}
		case <-tick.C:
			if len(pending) > 0 && flush(false) != nil {
				return
			}
		}
	}
}

func shouldStream(resp *http.Response) bool {
	if isSSE(resp) {
		return true
	}
	for _, te := range resp.TransferEncoding {
		if strings.EqualFold(te, "chunked") {
			return true
		}
	}
	return resp.ContentLength < 0 || resp.ContentLength > bufferedBodyLimit
}

func isSSE(resp *http.Response) bool {
	return strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

func isFiniteAsset(req *pv.PreviewRequest, method string) bool {
	if (method != http.MethodGet && method != http.MethodHead) || len(req.GetBody()) > 0 {
		return false
	}
	u, err := url.Parse(req.GetPath())
	if err != nil {
		return false
	}
	p := strings.ToLower(u.Path)
	if strings.HasPrefix(p, "/_next/static/") || p == "/_next/image" {
		return true
	}
	name := p[strings.LastIndex(p, "/")+1:]
	dot := strings.LastIndex(name, ".")
	return dot >= 0 && dot < len(name)-1 && finiteAssets[name[dot+1:]]
}

func (l *link) sendResponse(id string, r *pv.PreviewResponse) error {
	return l.send(&gw.ClientMessage{Body: &gw.ClientMessage_HttpResponse{HttpResponse: &gw.HttpResponse{SessionId: id, Response: r}}})
}

// sendError is rendered by the gateway with its own page (x-repogo-gateway-error).
func (l *link) sendError(id string, status uint32, message, title string) {
	_ = l.sendResponse(id, &pv.PreviewResponse{
		Status:      status,
		HeaderLines: []string{"content-type: text/plain", "x-repogo-gateway-error: " + title},
		Body:        []byte(message),
	})
}

// ---- WebSocket ----

type wsSession struct {
	conn *websocket.Conn
	slug string
	once sync.Once
}

func (w *wsSession) close(code websocket.StatusCode, reason string) {
	w.once.Do(func() { _ = w.conn.Close(code, reason) })
}

func (l *link) openWS(ctx context.Context, open *pv.PreviewWsOpen) {
	id := open.GetSessionId()
	ack := func(code uint32, reason, proto string) {
		_ = l.send(&gw.ClientMessage{Body: &gw.ClientMessage_WsOpenAck{WsOpenAck: &gw.WsOpenAck{
			SessionId: id, Ack: &pv.PreviewWsOpenAck{AcceptedSubprotocol: proto, CloseCode: code, CloseReason: reason},
		}}})
	}
	if id == "" {
		return
	}
	base, public, ok := l.resolve(open.GetTarget(), open.GetHeaderLines())
	if !ok {
		ack(uint32(websocket.StatusPolicyViolation), "tunnel closed", "")
		return
	}
	path, ok := requestPath(open.GetPath())
	if !ok {
		ack(uint32(websocket.StatusPolicyViolation), "invalid path", "")
		return
	}
	header := http.Header{}
	copyHeaders(header, open.GetHeaderLines(), wsHandshake)
	forwarded(header, public)
	var protos []string
	for _, p := range open.GetSubprotocols() {
		if p != "" {
			protos = append(protos, p)
		}
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(base, "http")+path, &websocket.DialOptions{
		HTTPClient: loopback, HTTPHeader: header, Subprotocols: protos,
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		ack(uint32(websocket.StatusInternalError), "local connect failed", "")
		return
	}
	conn.SetReadLimit(wsReadLimit)
	slug, _, _ := strings.Cut(public, ".")
	w := &wsSession{conn: conn, slug: slug}
	l.mu.Lock()
	l.ws[id] = w
	l.mu.Unlock()
	// The gateway forwards frames the moment it sees the ack, so only now.
	ack(0, "", conn.Subprotocol())

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			l.mu.Lock()
			_, known := l.ws[id]
			delete(l.ws, id)
			l.mu.Unlock()
			if known {
				code := websocket.CloseStatus(err)
				if code == -1 {
					code = websocket.StatusNormalClosure
				}
				_ = l.send(&gw.ClientMessage{Body: &gw.ClientMessage_WsClose{WsClose: &pv.PreviewWsClose{SessionId: id, Code: uint32(code)}}})
			}
			w.close(websocket.StatusNormalClosure, "")
			return
		}
		frame := pv.WsFrameType_WS_FRAME_TYPE_TEXT
		if typ == websocket.MessageBinary {
			frame = pv.WsFrameType_WS_FRAME_TYPE_BINARY
		}
		_ = l.send(&gw.ClientMessage{Body: &gw.ClientMessage_WsFrame{WsFrame: &pv.PreviewWsFrame{SessionId: id, Type: frame, Data: data}}})
	}
}

func (l *link) session(id string) *wsSession {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ws[id]
}

func (l *link) frameWS(ctx context.Context, f *pv.PreviewWsFrame) {
	w := l.session(f.GetSessionId())
	if w == nil {
		return
	}
	typ := websocket.MessageText
	if f.GetType() == pv.WsFrameType_WS_FRAME_TYPE_BINARY {
		typ = websocket.MessageBinary
	}
	if err := w.conn.Write(ctx, typ, f.GetData()); err != nil && !errors.Is(err, context.Canceled) {
		l.s.cfg.Log.Debug("tunnel: websocket write failed", "err", err)
	}
}

func (l *link) closeWS(c *pv.PreviewWsClose) {
	l.mu.Lock()
	w := l.ws[c.GetSessionId()]
	delete(l.ws, c.GetSessionId())
	l.mu.Unlock()
	if w == nil {
		return
	}
	code := websocket.StatusCode(c.GetCode())
	if code < 1000 {
		code = websocket.StatusNormalClosure
	}
	w.close(code, c.GetReason())
}

// closeSlug ends the WebSockets a closed tunnel still carries.
func (l *link) closeSlug(slug string) {
	l.mu.Lock()
	var closing []*wsSession
	for id, w := range l.ws {
		if w.slug == slug {
			closing = append(closing, w)
			delete(l.ws, id)
		}
	}
	l.mu.Unlock()
	for _, w := range closing {
		w.close(websocket.StatusGoingAway, "tunnel closed")
	}
}

func (l *link) closeAll() {
	l.mu.Lock()
	sessions := l.ws
	l.ws = map[string]*wsSession{}
	l.mu.Unlock()
	for _, w := range sessions {
		w.close(websocket.StatusGoingAway, "tunnel closed")
	}
}
