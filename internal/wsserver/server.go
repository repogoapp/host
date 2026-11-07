// Package wsserver is the loopback WebSocket server. Any web page can reach
// localhost, so it checks Host, Origin, and the 0600 token.
package wsserver

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/handshake"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/rpc"
	"github.com/repogoapp/host/internal/wirecodec"
	"github.com/repogoapp/host/internal/wsconn"
)

type Config struct {
	// Port to bind. Zero picks a free one, published in the runtime file.
	Port int

	// Shared secret from the 0600 runtime file.
	Token string

	// Identifies this runtime in the challenge a client signs. Stable across restarts.
	ServerID string

	// Paired devices, verified against a Hello's challenge signature.
	Devices *device.Store

	// Additional routes (the HTTP API) mounted under "/", behind the same guard.
	Mux *http.ServeMux

	// SelfAuthed are routes that authenticate their own callers, by path:
	// RepoGo's MCP server, whose callers are the agents this host spawned and
	// hold a bearer of its own, not the runtime token. Host is still checked.
	SelfAuthed map[string]http.Handler

	// Router is the host's vocabulary, on loopback and through the relay alike.
	Router *rpc.Router

	// Pushes is where a connected device is registered so unsolicited events
	// reach it over its socket.
	Pushes *emit.Mux

	// OnDisconnect runs when a device's last socket here closes.
	OnDisconnect func(device.ID)

	// Pairing reports whether a pairing code is live. Only then may a device
	// this host doesn't know say hello over passthrough, to call pair.complete.
	Pairing func() bool

	Log *slog.Logger
}

type Server struct {
	cfg  Config
	log  *slog.Logger
	http *http.Server

	mu sync.Mutex
	// remote is each ServeRemote running: the relay's, and the tailnet's.
	remote map[*http.Server]struct{}
	conns  map[device.ID]*conn
	// open is every socket past its hello, a device's superseded ones included.
	open map[*conn]struct{}
}

// maxUnproven caps one listener's connections before their hello, so streams
// a stranger opens and holds can't pile up; each lasts remoteHelloTimeout.
var maxUnproven int32 = 64

// maxOpen caps one listener's sockets, counted before TLS, so a flood on the
// LAN can't hold the relay's or the tailnet's connections hostage.
var maxOpen = 256

func New(cfg Config) (*Server, error) {
	if cfg.Token == "" {
		return nil, errors.New("wsserver: token is required")
	}
	if cfg.ServerID == "" {
		return nil, errors.New("wsserver: server id is required")
	}
	if cfg.Router == nil || cfg.Devices == nil || cfg.Pushes == nil || cfg.OnDisconnect == nil {
		return nil, errors.New("wsserver: router, devices, pushes and on-disconnect are required")
	}
	s := &Server{cfg: cfg, log: cfg.Log, conns: map[device.ID]*conn{}, open: map[*conn]struct{}{}, remote: map[*http.Server]struct{}{}}
	cfg.Devices.OnRevoke(s.revoked)
	return s, nil
}

// Online reports whether a device has a socket here.
func (s *Server) Online(id device.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[id] != nil
}

// revoked closes every socket a removed device holds; its calls end with them.
func (s *Server) revoked(id device.ID) {
	s.mu.Lock()
	var gone []*conn
	for c := range s.open {
		if c.caller.Device == id {
			gone = append(gone, c)
		}
	}
	s.mu.Unlock()
	for _, c := range gone {
		c.ws.CloseNow()
	}
}

// Listen binds and serves; the address tells a caller that passed Port 0 the
// real port to publish.
func (s *Server) Listen() (net.Addr, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	if s.cfg.Mux != nil {
		mux.Handle("/", s.guard(s.cfg.Mux))
	}
	for path, h := range s.cfg.SelfAuthed {
		mux.Handle(path, loopbackOnly(h))
	}
	srv, addr, err := wsconn.Listen(fmt.Sprintf("127.0.0.1:%d", s.cfg.Port), mux, s.log)
	if err != nil {
		return nil, fmt.Errorf("wsserver: listen: %w", err)
	}
	s.http = srv
	return addr, nil
}

// CloseRemote drops every passthrough socket, as after a TLS key rotation:
// each phone reconnects and meets the new key.
func (s *Server) CloseRemote() {
	s.mu.Lock()
	var remote []*conn
	for c := range s.open {
		if c.bind != nil {
			remote = append(remote, c)
		}
	}
	s.mu.Unlock()
	for _, c := range remote {
		c.ws.CloseNow()
	}
}

// ServeRemote ends pinned TLS with keys on lis, the relay's or a direct one,
// and serves /ws only to paired devices whose hello binds that session. Each
// listener has its own limits, so one flooded can't starve another.
func (s *Server) ServeRemote(lis net.Listener, keys *tls.Config) error {
	var unproven atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) { s.handleRemoteWS(w, r, &unproven) })
	lis = tls.NewListener(&capped{Listener: lis, open: make(chan struct{}, maxOpen)}, keys)
	// Short, as the TLS handshake shares it: a phone sends its request at once.
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: remoteHelloTimeout, ErrorLog: slog.NewLogLogger(s.log.Handler(), slog.LevelDebug)}
	s.mu.Lock()
	s.remote[srv] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.remote, srv)
		s.mu.Unlock()
	}()
	err := srv.Serve(lis)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	remote := make([]*http.Server, 0, len(s.remote))
	for srv := range s.remote {
		remote = append(remote, srv)
	}
	s.mu.Unlock()
	for _, srv := range remote {
		srv.Shutdown(ctx)
	}
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
}

// handleRemoteWS is /ws over passthrough TLS. Its Host is the relay name, not
// loopback, so the loopback check is replaced by the TLS binding: the hello
// must sign this session's exporter, and the local token is never accepted.
func (s *Server) handleRemoteWS(w http.ResponseWriter, r *http.Request, unproven *atomic.Int32) {
	if r.TLS == nil {
		http.Error(w, "TLS required", http.StatusForbidden)
		return
	}
	exporter, err := r.TLS.ExportKeyingMaterial(device.TLSExporterLabel, nil, device.TLSExporterLen)
	if err != nil {
		http.Error(w, "no TLS exporter", http.StatusForbidden)
		return
	}
	if unproven.Add(1) > maxUnproven {
		unproven.Add(-1)
		http.Error(w, "too many connections waiting", http.StatusServiceUnavailable)
		return
	}
	var proven sync.Once
	done := func() { proven.Do(func() { unproven.Add(-1) }) }
	defer done()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		s.log.Warn("wsserver: remote accept failed", "err", err)
		return
	}
	c.SetReadLimit(jsonrpc.MaxMessageBytes + wirecodec.Overhead)
	conn := newConn(s, c)
	conn.bind = exporter
	conn.helloDone = done
	conn.run(r.Context())
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackHost(r.Host) {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}

	// Origin only stops a browser from silently upgrading; a native client sends none.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns:  []string{"localhost:*", "127.0.0.1:*", "[::1]:*"},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("wsserver: accept failed", "err", err)
		return
	}
	c.SetReadLimit(jsonrpc.MaxMessageBytes + wirecodec.Overhead)

	newConn(s, c).run(r.Context())
}

// attach makes c the device's current socket and routes its pushes here. It
// reports false for a device removed after its hello, which revoked missed.
func (s *Server) attach(c *conn) bool {
	s.mu.Lock()
	s.conns[c.caller.Device] = c
	s.open[c] = struct{}{}
	s.mu.Unlock()
	s.cfg.Pushes.Attach(c.caller.Device, c)
	if c.caller.Scope != rpc.ScopeRemote {
		return true
	}
	_, err := s.cfg.Devices.Peer(c.caller.Device)
	return err == nil
}

// detach ignores a socket that closes after its successor attached, so the
// successor keeps its route and subscriptions.
func (s *Server) detach(c *conn) {
	s.cfg.Pushes.Detach(c.caller.Device, c)
	s.mu.Lock()
	delete(s.open, c)
	last := s.conns[c.caller.Device] == c
	if last {
		delete(s.conns, c.caller.Device)
	}
	s.mu.Unlock()
	if last {
		s.cfg.OnDisconnect(c.caller.Device)
	}
}

// isLoopbackHost matches on the name, not the resolved address: DNS rebinding
// works precisely because a hostile name does resolve to 127.0.0.1.
func isLoopbackHost(host string) bool {
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		h = host
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	switch strings.ToLower(h) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// admitsStranger lets a device this host doesn't know through a passthrough
// hello while a pairing code is live: its key must sign the bound challenge
// and hash to its id, as a paired device's must.
func (s *Server) admitsStranger(h handshake.Hello, signed []byte) bool {
	if s.cfg.Pairing == nil || !s.cfg.Pairing() {
		return false
	}
	if _, err := s.cfg.Devices.Peer(device.ID(h.DeviceID)); err == nil {
		return false
	}
	return device.ValidPhoneKey(h.PublicKey) && device.IDFor(h.PublicKey) == device.ID(h.DeviceID) &&
		device.VerifyPhone(h.PublicKey, signed, h.ChallengeSig)
}

func (s *Server) tokenValid(got string) bool {
	return subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.Token)) == 1
}

// loopbackOnly is guard without the token, for a route that checks its own.
func loopbackOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// guard applies the WebSocket handshake's two checks to plain HTTP: loopback
// Host, and the token a web page cannot read.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if !s.tokenExempt(r.URL.Path) && !s.tokenValid(requestToken(r)) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// tokenExempt is a method a joining device may reach without the token, as
// /v1/rpc/<family>/<method>: the router says which, guarded by the pairing code.
func (s *Server) tokenExempt(path string) bool {
	name, ok := strings.CutPrefix(path, "/v1/rpc/")
	return ok && s.cfg.Router.Unpaired(strings.Replace(name, "/", ".", 1))
}

func requestToken(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// authenticate accepts exactly one proof per connection and returns it as the
// scope: a signature is the only way to assert a paired device, the token
// pins the session to a local identity.
func (s *Server) authenticate(h *handshake.Hello, signed []byte) (rpc.Scope, error) {
	// Checked first so a client that can sign is never downgraded to token trust.
	if len(h.ChallengeSig) > 0 {
		if err := s.cfg.Devices.Verify(device.ID(h.DeviceID), signed, h.ChallengeSig); err != nil {
			return rpc.ScopeRemote, fmt.Errorf("device auth: %w", err)
		}
		// A signature never grants local scope, even from a device on this machine.
		return rpc.ScopeRemote, nil
	}

	// No signature: a same-machine client, which may not claim a paired identity.
	if !s.tokenValid(h.LocalToken) {
		return rpc.ScopeRemote, errors.New("invalid local token")
	}
	if _, err := s.cfg.Devices.Peer(device.ID(h.DeviceID)); err == nil {
		return rpc.ScopeRemote, errors.New("paired devices must authenticate by signature, not by token")
	}
	return rpc.ScopeLocal, nil
}

// capped closes connections past its capacity as they are accepted, before
// any TLS work, and frees a place when one closes.
type capped struct {
	net.Listener
	open chan struct{}
}

func (l *capped) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.open <- struct{}{}:
			return &cappedConn{Conn: c, free: func() { <-l.open }}, nil
		default:
			c.Close()
		}
	}
}

type cappedConn struct {
	net.Conn
	once sync.Once
	free func()
}

func (c *cappedConn) Close() error {
	c.once.Do(c.free)
	return c.Conn.Close()
}
