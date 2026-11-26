package tunnelgateway

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/testwait"
	"github.com/repogoapp/host/tunnelwire"
)

// Tunnels live at <slug>.repogo.test; hosts connect at tunnels.repogo.test.
const (
	domain    = "repogo.test"
	gatewayID = "tunnels." + domain
)

// rig is the gateway over real HTTP, with Upstash faked.
type rig struct {
	t      *testing.T
	server *Server
	url    string
	mu     sync.Mutex
	rows   map[string]string
	// lookups counts the rows asked of the fake Upstash.
	lookups int
	// cancels is what the hosts it connects say in their Hello.
	cancels bool
}

func newRig(t *testing.T) *rig {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := &rig{t: t, rows: map[string]string{}}
	upstash := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		row, ok := r.rows[strings.TrimPrefix(req.URL.Path, "/get/tunnel:")]
		r.lookups++
		r.mu.Unlock()
		if !ok {
			_, _ = w.Write([]byte(`{"result":null}`))
			return
		}
		b, _ := json.Marshal(map[string]string{"result": row})
		_, _ = w.Write(b)
	}))
	t.Cleanup(upstash.Close)
	r.server = &Server{
		Domain: domain, GatewayID: gatewayID,
		Registry: NewUpstash(upstash.URL, "token"),
		Hub:      NewHub(log), Cache: NewResponseCache(), Log: log,
	}
	srv := httptest.NewServer(r.server.Handler())
	t.Cleanup(srv.Close)
	r.url = srv.URL
	return r
}

func (r *rig) row(slug string, t Tunnel) {
	b, _ := json.Marshal(t)
	r.mu.Lock()
	r.rows[slug] = string(b)
	r.mu.Unlock()
}

func inAnHour() int64 { return time.Now().Add(time.Hour).UnixMilli() }

// do sends a visitor request for host through the gateway.
func (r *rig) do(method, host string, body io.Reader) *http.Response {
	r.t.Helper()
	req, err := http.NewRequest(method, r.url+"/page?q=1", body)
	if err != nil {
		r.t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Accept", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func read(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// testHost is a host's side of the socket: it proves a key, then answers
// requests with handle.
type testHost struct {
	id   string
	conn *websocket.Conn
	ctx  context.Context
	mu   sync.Mutex
}

func (r *rig) connect(priv ed25519.PrivateKey, signFor string) (*testHost, error) {
	ctx, cancel := context.WithCancel(context.Background())
	r.t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.url, "http")+tunnelwire.ConnectPath, &websocket.DialOptions{Host: gatewayID})
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(tunnelwire.MaxMessage)
	h := &testHost{id: hostIDFor(priv.Public().(ed25519.PublicKey)), conn: conn, ctx: ctx}
	if err := h.send(&tunnelwire.Hello{PublicKey: priv.Public().(ed25519.PublicKey), CancelsHTTP: r.cancels}); err != nil {
		return nil, err
	}
	m, err := h.read()
	if err != nil {
		return nil, err
	}
	c := m.(*tunnelwire.Challenge)
	if err := h.send(&tunnelwire.Proof{Signature: ed25519.Sign(priv, tunnelwire.ProofMessage(c.Nonce, signFor, c.Time))}); err != nil {
		return nil, err
	}
	if m, err = h.read(); err != nil {
		return nil, err
	}
	if _, ok := m.(*tunnelwire.Ready); !ok {
		r.t.Fatalf("expected ready, got %T", m)
	}
	return h, nil
}

func (h *testHost) send(m tunnelwire.Message) error {
	b, err := tunnelwire.Encode(m)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.conn.Write(h.ctx, websocket.MessageBinary, b)
}

func (h *testHost) read() (tunnelwire.Message, error) {
	_, b, err := h.conn.Read(h.ctx)
	if err != nil {
		return nil, err
	}
	return tunnelwire.Decode(b)
}

// serve answers every request with body, echoing the target and the Host line
// it was given, plus a cookie scoped to the whole domain.
func (h *testHost) serve(body string) {
	go func() {
		for {
			m, err := h.read()
			if err != nil {
				return
			}
			req, ok := m.(*tunnelwire.Request)
			if !ok {
				continue
			}
			_ = h.send(&tunnelwire.Response{SessionID: req.SessionID, Status: 200, Body: []byte(body), HeaderLines: []string{
				"X-Target: " + req.Target,
				"X-Host: " + hostLineOf(req.HeaderLines),
				"X-Path: " + req.Path,
				"Set-Cookie: sid=1; Domain=" + domain + "; Path=/; HttpOnly",
			}})
		}
	}()
}

func hostLineOf(lines []string) string {
	out := ""
	for _, l := range lines {
		if k, v, ok := strings.Cut(l, ": "); ok && strings.EqualFold(k, "host") {
			out += v
		}
	}
	return out
}

func (r *rig) waitHost(id string) {
	r.t.Helper()
	testwait.For(r.t, "the host to register", func() bool {
		_, err := r.server.Hub.Lookup(id)
		return err == nil
	})
}

func (r *rig) host(body string) *testHost {
	r.t.Helper()
	_, priv, _ := ed25519.GenerateKey(nil)
	h, err := r.connect(priv, gatewayID)
	if err != nil {
		r.t.Fatal(err)
	}
	h.serve(body)
	r.waitHost(h.id)
	return h
}

func TestServesTheProvenHost(t *testing.T) {
	r := newRig(t)
	h := r.host("hello")
	r.row("abcdefghij12", Tunnel{HostID: h.id, Target: "3000", Enabled: true, ExpiresAt: inAnHour()})

	resp := r.do(http.MethodGet, "abcdefghij12."+domain, nil)
	if body := read(t, resp); resp.StatusCode != 200 || body != "hello" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Target"); got != "3000" {
		t.Fatalf("target %q", got)
	}
	// The host learns which public name was asked for, and only that.
	if got := resp.Header.Get("X-Host"); got != "abcdefghij12."+domain {
		t.Fatalf("host line %q", got)
	}
	if got := resp.Header.Get("X-Path"); got != "/page?q=1" {
		t.Fatalf("path %q", got)
	}
	if c := resp.Header.Get("Set-Cookie"); strings.Contains(c, "Domain") || !strings.Contains(c, "sid=1") {
		t.Fatalf("cookie %q", c)
	}
}

func TestForgedProofIsRefused(t *testing.T) {
	r := newRig(t)
	_, other, _ := ed25519.GenerateKey(nil)
	pub, _, _ := ed25519.GenerateKey(nil)
	// Claims pub but signs with another key.
	ctx := t.Context()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.url, "http")+tunnelwire.ConnectPath, &websocket.DialOptions{Host: gatewayID})
	if err != nil {
		t.Fatal(err)
	}
	h := &testHost{conn: conn, ctx: ctx}
	_ = h.send(&tunnelwire.Hello{PublicKey: pub})
	m, err := h.read()
	if err != nil {
		t.Fatal(err)
	}
	c := m.(*tunnelwire.Challenge)
	_ = h.send(&tunnelwire.Proof{Signature: ed25519.Sign(other, tunnelwire.ProofMessage(c.Nonce, c.GatewayID, c.Time))})
	if _, err := h.read(); websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("got %v, want a policy close", err)
	}
	if _, err := r.server.Hub.Lookup(hostIDFor(pub)); err == nil {
		t.Fatal("forged host registered")
	}
}

func TestProofForAnotherGatewayIsRefused(t *testing.T) {
	r := newRig(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	h, err := r.connect(priv, "gateway.elsewhere")
	if err == nil {
		_, err = h.read()
	}
	if err == nil {
		t.Fatal("accepted a proof signed for another gateway")
	}
}

func TestRowRefusals(t *testing.T) {
	r := newRig(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	hostID := hostIDFor(priv.Public().(ed25519.PublicKey))

	r.row("nohost123456", Tunnel{HostID: hostID, Target: "3000", Enabled: true, ExpiresAt: inAnHour()})
	if resp := r.do(http.MethodGet, "nohost123456."+domain, nil); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("no host: %d", resp.StatusCode)
	}

	h, err := r.connect(priv, gatewayID)
	if err != nil {
		t.Fatal(err)
	}
	h.serve("served")
	r.waitHost(hostID)

	cases := map[string]struct {
		row  *Tunnel
		want int
	}{
		"no row":              {nil, http.StatusNotFound},
		"a row with no host":  {&Tunnel{Target: "3000", Enabled: true, ExpiresAt: inAnHour()}, http.StatusNotFound},
		"expired":             {&Tunnel{HostID: hostID, Target: "3000", Enabled: true, ExpiresAt: time.Now().Add(-time.Second).UnixMilli()}, http.StatusGone},
		"no expiry":           {&Tunnel{HostID: hostID, Target: "3000", Enabled: true}, http.StatusGone},
		"disabled":            {&Tunnel{HostID: hostID, Target: "3000", ExpiresAt: inAnHour()}, http.StatusServiceUnavailable},
		"a hostname target":   {&Tunnel{HostID: hostID, Target: "web.localhost:3000", Enabled: true, ExpiresAt: inAnHour()}, http.StatusBadGateway},
		"a privileged port":   {&Tunnel{HostID: hostID, Target: "80", Enabled: true, ExpiresAt: inAnHour()}, http.StatusBadGateway},
		"a zero-padded port":  {&Tunnel{HostID: hostID, Target: "03000", Enabled: true, ExpiresAt: inAnHour()}, http.StatusBadGateway},
		"a port out of range": {&Tunnel{HostID: hostID, Target: "70000", Enabled: true, ExpiresAt: inAnHour()}, http.StatusBadGateway},
		"another host's row":  {&Tunnel{HostID: strings.Repeat("0", 32), Target: "3000", Enabled: true, ExpiresAt: inAnHour()}, http.StatusBadGateway},
	}
	i := 0
	for name, c := range cases {
		// A name of its own each, since a miss is remembered.
		i++
		slug := fmt.Sprintf("case%08d", i)
		r.mu.Lock()
		delete(r.rows, slug)
		r.mu.Unlock()
		if c.row != nil {
			r.row(slug, *c.row)
		}
		resp := r.do(http.MethodGet, slug+"."+domain, nil)
		if body := read(t, resp); resp.StatusCode != c.want || body == "served" {
			t.Errorf("%s: %d %q, want %d", name, resp.StatusCode, body, c.want)
		}
	}
}

func TestHostnames(t *testing.T) {
	r := newRig(t)
	for host, want := range map[string]int{
		"a.b." + domain:     http.StatusNotFound,
		"xn--abc." + domain: http.StatusNotFound,
		"other.example":     http.StatusNotFound,
		domain:              http.StatusNotFound,
		gatewayID:           http.StatusNotFound,
	} {
		if resp := r.do(http.MethodGet, host, nil); resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", host, resp.StatusCode, want)
		}
	}
}

func TestBodyCap(t *testing.T) {
	r := newRig(t)
	h := r.host("ok")
	r.row("bigbody12345", Tunnel{HostID: h.id, Target: "3000", Enabled: true, ExpiresAt: inAnHour()})
	resp := r.do(http.MethodPost, "bigbody12345."+domain, strings.NewReader(strings.Repeat("x", maxRequestBody+1)))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestRateLimit(t *testing.T) {
	r := newRig(t)
	r.server.Limits = NewRateLimits(100, 2, time.Minute)
	h := r.host("ok")
	r.row("limited12345", Tunnel{HostID: h.id, Target: "3000", Enabled: true, ExpiresAt: inAnHour()})
	for i, want := range []int{200, 200, http.StatusTooManyRequests} {
		if resp := r.do(http.MethodGet, "limited12345."+domain, nil); resp.StatusCode != want {
			t.Fatalf("request %d: %d, want %d", i, resp.StatusCode, want)
		}
	}
}

// A host that reconnects takes over; the old socket is closed.
func TestNewerSocketReplacesOlder(t *testing.T) {
	r := newRig(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	old, err := r.connect(priv, gatewayID)
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.connect(priv, gatewayID)
	if err != nil {
		t.Fatal(err)
	}
	h.serve("new")
	if _, err := old.read(); websocket.CloseStatus(err) != websocket.StatusGoingAway {
		t.Fatalf("old socket: %v", err)
	}
	r.row("replaced1234", Tunnel{HostID: h.id, Target: "3000", Enabled: true, ExpiresAt: inAnHour()})
	if resp := r.do(http.MethodGet, "replaced1234."+domain, nil); read(t, resp) != "new" {
		t.Fatal("not served by the newer socket")
	}
}

func TestStreamsBodies(t *testing.T) {
	r := newRig(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	h, err := r.connect(priv, gatewayID)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			m, err := h.read()
			if err != nil {
				return
			}
			if req, ok := m.(*tunnelwire.Request); ok {
				_ = h.send(&tunnelwire.Response{SessionID: req.SessionID, Status: 200, Streaming: true, HeaderLines: []string{"Content-Type: text/event-stream"}})
				_ = h.send(&tunnelwire.BodyChunk{SessionID: req.SessionID, Data: []byte("data: 1\n\n")})
				_ = h.send(&tunnelwire.BodyChunk{SessionID: req.SessionID, Data: []byte("data: 2\n\n"), EndOfStream: true})
			}
		}
	}()
	r.waitHost(h.id)
	r.row("streaming123", Tunnel{HostID: h.id, Target: "3000", Enabled: true, ExpiresAt: inAnHour()})
	resp := r.do(http.MethodGet, "streaming123."+domain, nil)
	if body := read(t, resp); body != "data: 1\n\ndata: 2\n\n" {
		t.Fatalf("%q", body)
	}
}

func TestBridgesWebSockets(t *testing.T) {
	r := newRig(t)
	_, priv, _ := ed25519.GenerateKey(nil)
	h, err := r.connect(priv, gatewayID)
	if err != nil {
		t.Fatal(err)
	}
	opened := make(chan *tunnelwire.WSOpen, 1)
	go func() {
		for {
			m, err := h.read()
			if err != nil {
				return
			}
			switch m := m.(type) {
			case *tunnelwire.WSOpen:
				opened <- m
				_ = h.send(&tunnelwire.WSOpenAck{SessionID: m.SessionID, Subprotocol: "vite-hmr"})
			case *tunnelwire.WSFrame:
				_ = h.send(&tunnelwire.WSFrame{SessionID: m.SessionID, Binary: m.Binary, Data: append([]byte("echo:"), m.Data...)})
			}
		}
	}()
	r.waitHost(h.id)
	r.row("sockets12345", Tunnel{HostID: h.id, Target: "5173", Enabled: true, ExpiresAt: inAnHour()})

	ctx := t.Context()
	visitor, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(r.url, "http")+"/hmr", &websocket.DialOptions{
		Host: "sockets12345." + domain, Subprotocols: []string{"vite-hmr"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer visitor.CloseNow()
	open := <-opened
	if open.Target != "5173" || open.Path != "/hmr" || hostLineOf(open.HeaderLines) != "sockets12345."+domain {
		t.Fatalf("open %+v", open)
	}
	if err := visitor.Write(ctx, websocket.MessageText, []byte("hi")); err != nil {
		t.Fatal(err)
	}
	typ, data, err := visitor.Read(ctx)
	if err != nil || typ != websocket.MessageText || string(data) != "echo:hi" {
		t.Fatalf("%v %v %q", err, typ, data)
	}
}

// The connect name sits where a slug would; a row named after it is never served.
func TestGatewayNameIsNeverATunnel(t *testing.T) {
	r := newRig(t)
	h := r.host("leaked")
	r.row("tunnels", Tunnel{HostID: h.id, Target: "3000", Enabled: true, ExpiresAt: inAnHour()})
	resp := r.do(http.MethodGet, gatewayID, nil)
	if body := read(t, resp); resp.StatusCode != http.StatusNotFound || body == "leaked" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
}

func TestHostIDMatchesTheDeviceID(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	if got, want := hostIDFor(pub), string(device.IDFor(pub)); got != want {
		t.Fatalf("hostIDFor = %s, device.IDFor = %s", got, want)
	}
}

func TestSlugOnlyOnTheDomain(t *testing.T) {
	s := &Server{Domain: "repogo.dev"}
	for host, want := range map[string]string{
		"abcdefghij12.repogo.dev":         "abcdefghij12",
		"abcdefghij12.tunnels.repogo.dev": "",
		"abcdefghij12.deeper.repogo.dev":  "",
		"abcdefghij12.example.com":        "",
	} {
		if got, _ := s.slug(host); got != want {
			t.Errorf("slug(%q) = %q, want %q", host, got, want)
		}
	}
}

// A visitor who hangs up before the answer cancels the request on a host that
// offered HTTPCancel.
func TestVisitorHangupCancelsOnTheHost(t *testing.T) {
	r := newRig(t)
	r.cancels = true
	_, priv, _ := ed25519.GenerateKey(nil)
	h, err := r.connect(priv, gatewayID)
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan tunnelwire.Message, 4)
	go func() {
		for {
			m, err := h.read()
			if err != nil {
				return
			}
			got <- m
		}
	}()
	next := func() tunnelwire.Message {
		t.Helper()
		select {
		case m := <-got:
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("nothing reached the host")
			return nil
		}
	}
	r.waitHost(h.id)
	r.row("hangup12345", Tunnel{HostID: h.id, Target: "3000", Enabled: true, ExpiresAt: inAnHour()})

	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url+"/slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "hangup12345." + domain
	go func() {
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	sent, ok := next().(*tunnelwire.Request)
	if !ok {
		t.Fatal("the request didn't reach the host")
	}
	cancel()
	if c, ok := next().(*tunnelwire.HTTPCancel); !ok || c.SessionID != sent.SessionID {
		t.Fatalf("got %+v, want HTTPCancel %s", c, sent.SessionID)
	}
}
