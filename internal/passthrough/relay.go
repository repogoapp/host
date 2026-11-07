// Package passthrough is the relay phones reach hosts through: phones open TLS
// to a host through it, and it only pipes bytes. It reads the ClientHello's
// server name to pick the host and never holds a key for that TLS. Hosts also
// send their pushes on their link, since the relay holds the APNs key.
package passthrough

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/pushgate"
)

// Relay routes a phone's TLS stream to the host its server name names:
// "<host id><Suffix>". The relay's own name, LinkName, ends TLS here instead,
// for hosts holding their link and answering dial notices.
type Relay struct {
	Suffix   string
	LinkName string
	LinkTLS  *tls.Config

	// ProxyProtocol reads a PROXY v1 line first, for the caller's address
	// behind Fly's proxy; per-address limits need it.
	ProxyProtocol bool
	// MaxPerIP caps one address's open connections, an IPv6 /64 counting as
	// one address; zero means no cap.
	MaxPerIP int
	// MaxWaitingPerHost caps the phone streams waiting on one host's dial-back,
	// since each costs that host a dial; a stream stops counting once the host
	// answers, so held streams can't fill it. Zero means no cap.
	MaxWaitingPerHost int
	// DialTimeout bounds how long a phone waits for its host to dial back.
	DialTimeout time.Duration
	// PingInterval spaces the pings on a host's link: they keep idle paths
	// open and let the host notice a dead relay. Zero sends none.
	PingInterval time.Duration
	// Push answers the push lines hosts send on their link; nil refuses them.
	Push *pushgate.Gate
	// Limits meter each host's phone streams: speed, a daily allowance and
	// an idle timeout. The zero value meters nothing.
	Limits Limits

	Log *slog.Logger

	mu      sync.Mutex
	links   map[device.ID]*link
	pending map[string]chan *replayConn
	perIP   map[string]int
	waiting map[device.ID]int
	meters  map[device.ID]*meter

	// clock and sleep stand in for time.Now and time.Sleep in tests.
	clock func() time.Time
	sleep func(time.Duration)
}

type link struct {
	mu    sync.Mutex
	conn  net.Conn
	enc   *json.Encoder
	pings int
}

// linkWriteTimeout bounds one write to a host, so a host that stops reading
// fails its link rather than holding every writer behind it.
const linkWriteTimeout = 10 * time.Second

// send writes one line to the host; pings, dial notices and push replies take turns.
func (l *link) send(line relayLine) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.conn.SetWriteDeadline(time.Now().Add(linkWriteTimeout))
	defer l.conn.SetWriteDeadline(time.Time{})
	err := l.enc.Encode(line)
	if err != nil {
		l.conn.Close()
	}
	return err
}

// Control lines are newline-delimited JSON inside the link TLS.
type hostLine struct {
	Op        string `json:"op"`
	Token     string `json:"token,omitempty"`
	DeviceID  string `json:"device_id,omitempty"`
	PublicKey []byte `json:"public_key,omitempty"`
	Signature []byte `json:"signature,omitempty"`
	// ID and Push are a push request, op "push"; the reply carries the same ID.
	ID   uint64                `json:"id,omitempty"`
	Push *pushgate.PushRequest `json:"push,omitempty"`
}

type relayLine struct {
	Nonce    []byte `json:"nonce,omitempty"`
	ServerID string `json:"server_id,omitempty"`
	WallMS   uint64 `json:"wall_ms,omitempty"`
	Dial     string `json:"dial,omitempty"`
	OK       bool   `json:"ok,omitempty"`
	Ping     bool   `json:"ping,omitempty"`
	Error    string `json:"error,omitempty"`
	// ID answers the push request with that ID; Code is its JSON-RPC error code.
	ID   uint64 `json:"id,omitempty"`
	Code int    `json:"code,omitempty"`
}

const (
	// maxPushes bounds one link's push requests in flight.
	maxPushes = 16
	// pushTimeout bounds one push request, APNs included.
	pushTimeout = 10 * time.Second
)

var errPeeked = errors.New("passthrough: client hello read")

// Serve accepts connections until ctx ends or lis fails.
func (r *Relay) Serve(ctx context.Context, lis net.Listener) error {
	go func() { <-ctx.Done(); lis.Close() }()
	for {
		c, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go r.handle(ctx, c)
	}
}

// Linked reports whether a host holds a link here.
func (r *Relay) Linked(id device.ID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.links[id] != nil
}

func (r *Relay) handle(ctx context.Context, c net.Conn) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(c)
	addr := c.RemoteAddr().String()
	if r.ProxyProtocol {
		var err error
		if addr, err = readProxyLine(reader); err != nil {
			r.Log.Debug("passthrough: no PROXY line", "err", err)
			return
		}
	}
	ip := addressKey(hostOnly(addr))
	if !r.admit(ip) {
		r.Log.Debug("passthrough: address over its cap", "ip", ip)
		return
	}
	defer r.release(ip)

	name, hello, err := peekServerName(reader)
	if err != nil {
		r.Log.Debug("passthrough: no client hello", "ip", ip, "err", err)
		return
	}
	stream := &replayConn{Conn: c, r: io.MultiReader(bytes.NewReader(hello), reader)}
	switch {
	case strings.EqualFold(name, r.LinkName):
		r.serveLink(ctx, stream)
	case strings.HasSuffix(strings.ToLower(name), strings.ToLower(r.Suffix)):
		label := name[:len(name)-len(r.Suffix)]
		r.servePhone(ctx, stream, device.ID(strings.ToLower(label)))
	default:
		r.Log.Debug("passthrough: unknown server name", "name", name)
	}
}

// servePhone asks the named host to dial back and splices the two streams.
func (r *Relay) servePhone(ctx context.Context, phone *replayConn, id device.ID) {
	r.mu.Lock()
	l := r.links[id]
	r.mu.Unlock()
	if l == nil {
		// Closing is all the relay can do: it can't speak inside the phone's TLS.
		return
	}
	if r.overCap(id) {
		// Logged once when it passed the cap; refused quietly until midnight UTC.
		return
	}
	token := newToken()
	ready := make(chan *replayConn, 1)
	r.mu.Lock()
	if r.MaxWaitingPerHost > 0 && r.waiting[id] >= r.MaxWaitingPerHost {
		r.mu.Unlock()
		r.Log.Debug("passthrough: host has too many streams waiting", "host", id)
		return
	}
	r.waiting[id]++
	r.pending[token] = ready
	r.mu.Unlock()
	var once sync.Once
	stopWaiting := func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.pending, token)
			if r.waiting[id]--; r.waiting[id] <= 0 {
				delete(r.waiting, id)
			}
			r.mu.Unlock()
		})
	}
	defer stopWaiting()
	if err := l.send(relayLine{Dial: token}); err != nil {
		return
	}
	select {
	case host := <-ready:
		// Answered: no longer waiting, however long the stream then lasts.
		stopWaiting()
		defer host.Close()
		phone.SetReadDeadline(time.Time{})
		r.splice(id, phone, host)
	case <-time.After(r.DialTimeout):
		r.Log.Debug("passthrough: host did not dial back", "host", id)
	case <-ctx.Done():
	}
}

// serveLink ends the relay's own TLS and reads one control line: a host
// opening its link, or answering a dial notice.
func (r *Relay) serveLink(ctx context.Context, raw net.Conn) {
	conn := tls.Server(raw, r.LinkTLS)
	if err := conn.HandshakeContext(ctx); err != nil {
		return
	}
	reader := bufio.NewReader(conn)
	enc := json.NewEncoder(conn)
	var first hostLine
	if err := readLine(reader, &first); err != nil {
		return
	}
	switch first.Op {
	case "link":
		r.holdLink(ctx, conn, reader, enc)
	case "accept":
		r.mu.Lock()
		ready := r.pending[first.Token]
		delete(r.pending, first.Token)
		r.mu.Unlock()
		if ready == nil {
			enc.Encode(relayLine{Error: "no such stream"})
			return
		}
		if err := enc.Encode(relayLine{OK: true}); err != nil {
			return
		}
		conn.SetReadDeadline(time.Time{})
		done := make(chan struct{})
		ready <- &replayConn{Conn: conn, r: reader, closed: done}
		<-done
	}
}

// holdLink proves the host holds its identity key, then keeps the link for
// dial notices and the host's push requests until it drops.
func (r *Relay) holdLink(ctx context.Context, conn *tls.Conn, reader *bufio.Reader, enc *json.Encoder) {
	nonce := make([]byte, device.NonceLen)
	rand.Read(nonce)
	wall := uint64(time.Now().UnixMilli())
	if err := enc.Encode(relayLine{Nonce: nonce, ServerID: r.LinkName, WallMS: wall}); err != nil {
		return
	}
	var hello hostLine
	if err := readLine(reader, &hello); err != nil {
		return
	}
	id, err := verifyHost(hello, nonce, r.LinkName, wall)
	if err != nil {
		enc.Encode(relayLine{Error: err.Error()})
		return
	}
	l := &link{conn: conn, enc: enc}
	r.mu.Lock()
	r.links[id] = l
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.links[id] == l {
			delete(r.links, id)
		}
		r.mu.Unlock()
	}()
	if err := l.send(relayLine{OK: true}); err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})
	defer context.AfterFunc(ctx, func() { conn.Close() })()
	if r.PingInterval > 0 {
		stop := make(chan struct{})
		defer close(stop)
		go r.ping(l, stop)
	}
	r.readPushes(ctx, id, reader, l)
}

// readPushes answers the host's push lines until the link drops. Each runs on
// its own goroutine, so a slow APNs never holds up the next.
func (r *Relay) readPushes(ctx context.Context, id device.ID, reader *bufio.Reader, l *link) {
	var window pushgate.Window
	inFlight := make(chan struct{}, maxPushes)
	for {
		var line hostLine
		if err := readLine(reader, &line); err != nil {
			return
		}
		if line.Op != "push" || line.Push == nil {
			continue
		}
		switch {
		case r.Push == nil:
			l.send(relayLine{ID: line.ID, Code: jsonrpc.CodeUnavailable, Error: "this relay has no APNs key"})
			continue
		case !window.Allow(pushgate.HostBurst):
			l.send(relayLine{ID: line.ID, Code: jsonrpc.CodeDenied, Error: "push rate limit"})
			continue
		}
		select {
		case inFlight <- struct{}{}:
		default:
			l.send(relayLine{ID: line.ID, Code: jsonrpc.CodeUnavailable, Error: "too many pushes in flight"})
			continue
		}
		go func() {
			defer func() { <-inFlight }()
			ctx, cancel := context.WithTimeout(ctx, pushTimeout)
			defer cancel()
			reply := relayLine{ID: line.ID, OK: true}
			if err := r.Push.Send(ctx, id, *line.Push); err != nil {
				reply = relayLine{ID: line.ID, Code: jsonrpc.CodeInternal, Error: err.Error()}
				var rpcErr *jsonrpc.Error
				if errors.As(err, &rpcErr) {
					reply.Code, reply.Error = rpcErr.Code, rpcErr.Message
				}
				if reply.Code == jsonrpc.CodeInternal {
					r.Log.Warn("passthrough: push failed", "host", id, "err", err)
				}
			}
			l.send(reply)
		}()
	}
}

// ping writes on the link until it stops or a write fails; a failed write
// closes the link, so the read in holdLink returns and the link is dropped.
func (r *Relay) ping(l *link, stop <-chan struct{}) {
	t := time.NewTicker(r.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			err := l.send(relayLine{Ping: true})
			l.mu.Lock()
			l.pings++
			l.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func verifyHost(h hostLine, nonce []byte, serverID string, wall uint64) (device.ID, error) {
	pub := ed25519.PublicKey(h.PublicKey)
	if len(pub) != ed25519.PublicKeySize || device.IDFor(pub) != device.ID(h.DeviceID) {
		return "", errors.New("device id does not match its public key")
	}
	msg, err := device.ChallengeMessage(nonce, serverID, wall)
	if err != nil {
		return "", err
	}
	if !ed25519.Verify(pub, msg, h.Signature) {
		return "", errors.New("signature does not verify")
	}
	return device.ID(h.DeviceID), nil
}

func (r *Relay) admit(ip string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.links == nil {
		r.links = map[device.ID]*link{}
		r.pending = map[string]chan *replayConn{}
		r.perIP = map[string]int{}
		r.waiting = map[device.ID]int{}
	}
	if r.MaxPerIP > 0 && r.perIP[ip] >= r.MaxPerIP {
		return false
	}
	r.perIP[ip]++
	return true
}

func (r *Relay) release(ip string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.perIP[ip]--; r.perIP[ip] <= 0 {
		delete(r.perIP, ip)
	}
}

// peekServerName reads the ClientHello and returns its server name with every
// byte read, so the stream can be replayed untouched to whoever ends the TLS.
func peekServerName(r io.Reader) (string, []byte, error) {
	var seen bytes.Buffer
	var name string
	called := false
	err := tls.Server(readOnly{io.TeeReader(r, &seen)}, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			name, called = h.ServerName, true
			return nil, errPeeked
		},
	}).Handshake()
	if !called {
		return "", nil, fmt.Errorf("passthrough: not a TLS client hello: %w", err)
	}
	if name == "" {
		return "", nil, errors.New("passthrough: client hello has no server name")
	}
	return name, seen.Bytes(), nil
}

// readLine reads one control line whole, so nothing after it is buffered away
// from the stream that follows.
func readLine(r *bufio.Reader, v any) error {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		// Capped: the relay reads a link's first line before anyone has signed in.
		if len(line) > maxLine {
			return errLineTooLong
		}
		if err == nil {
			return json.Unmarshal(line, v)
		}
		if err != bufio.ErrBufferFull {
			return err
		}
	}
}

// maxLine bounds one control line; the largest real one, a signed hello, is
// well under 1 KB.
const maxLine = 64 << 10

var errLineTooLong = errors.New("passthrough: control line too long")

func readProxyLine(r *bufio.Reader) (string, error) {
	// Checked first, so a caller that skipped the line fails now, not at a newline.
	if head, err := r.Peek(6); err != nil || string(head) != "PROXY " {
		return "", errors.New("passthrough: no PROXY line")
	}
	// A PROXY v1 line is at most 107 bytes, well inside one buffer.
	raw, err := r.ReadSlice('\n')
	if err != nil {
		return "", err
	}
	line := string(raw)
	f := strings.Fields(line)
	if len(f) < 6 || f[0] != "PROXY" {
		return "", fmt.Errorf("passthrough: bad PROXY line %q", line)
	}
	return net.JoinHostPort(f[2], f[4]), nil
}

func newToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// addressKey is what MaxPerIP counts: the address, or an IPv6 address's /64,
// which one machine is usually handed whole.
func addressKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil || !addr.Is6() || addr.Is4In6() {
		return ip
	}
	prefix, err := addr.Prefix(64)
	if err != nil {
		return ip
	}
	return prefix.String()
}

func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// replayConn reads from r, which starts with bytes already taken off the
// socket, and signals closed once, when its user closes it.
type replayConn struct {
	net.Conn
	r      io.Reader
	closed chan struct{}
	once   sync.Once
}

func (c *replayConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// CloseWrite ends one direction of a splice and leaves the other running.
func (c *replayConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}

func (c *replayConn) Close() error {
	c.once.Do(func() {
		if c.closed != nil {
			close(c.closed)
		}
	})
	return c.Conn.Close()
}

// readOnly lets tls.Server parse a ClientHello it must never answer.
type readOnly struct{ r io.Reader }

func (c readOnly) Read(p []byte) (int, error)     { return c.r.Read(p) }
func (readOnly) Write(p []byte) (int, error)      { return 0, errPeeked }
func (readOnly) Close() error                     { return nil }
func (readOnly) LocalAddr() net.Addr              { return nil }
func (readOnly) RemoteAddr() net.Addr             { return nil }
func (readOnly) SetDeadline(time.Time) error      { return nil }
func (readOnly) SetReadDeadline(time.Time) error  { return nil }
func (readOnly) SetWriteDeadline(time.Time) error { return nil }
