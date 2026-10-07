// Package wsserver is the loopback WebSocket server. Any web page can reach
// localhost, so it checks Host, Origin, and the 0600 token.
package wsserver

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/handshake"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/wsconn"
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

	// Router is the host's vocabulary, shared with the relay path.
	Router *rpc.Router

	// Pushes is where a connected device is registered so unsolicited events
	// reach it over this socket rather than the relay.
	Pushes *emit.Mux

	// OnDisconnect runs when a device's last socket here closes.
	OnDisconnect func(device.ID)

	Log *slog.Logger
}

type Server struct {
	cfg  Config
	log  *slog.Logger
	http *http.Server

	mu    sync.Mutex
	conns map[device.ID]*conn
	// open is every socket past its hello, a device's superseded ones included.
	open map[*conn]struct{}
}

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
	s := &Server{cfg: cfg, log: cfg.Log, conns: map[device.ID]*conn{}, open: map[*conn]struct{}{}}
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

func (s *Server) Shutdown(ctx context.Context) error {
	if s.http == nil {
		return nil
	}
	return s.http.Shutdown(ctx)
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
	c.SetReadLimit(jsonrpc.MaxMessageBytes)

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
