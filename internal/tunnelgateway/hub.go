package tunnelgateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/repogoapp/host/tunnelwire"
)

var (
	ErrNoHost     = errors.New("host not connected")
	ErrHostClosed = errors.New("host disconnected")
	ErrHostBusy   = errors.New("host busy")
)

const (
	httpTimeout   = 60 * time.Second
	wsOpenTimeout = 15 * time.Second
	// Chunks are at most maxChunk, so a session buffers at most 32 MB before
	// a slow visitor costs it the session.
	chunkBuffer = 32
	frameBuffer = 64
)

type Hub struct {
	log   *slog.Logger
	mu    sync.RWMutex
	hosts map[string]*Host
}

func NewHub(log *slog.Logger) *Hub {
	return &Hub{log: log, hosts: map[string]*Host{}}
}

// Register makes h the host's socket, closing any older one: the last to
// connect wins. The channel closes when h is replaced or unregistered.
func (hub *Hub) Register(h *Host) <-chan struct{} {
	hub.mu.Lock()
	if old := hub.hosts[h.ID]; old != nil {
		old.evict("replaced by a newer connection")
	}
	hub.hosts[h.ID] = h
	hub.mu.Unlock()
	return h.evicted
}

// Unregister removes h unless a newer socket already took its place.
func (hub *Hub) Unregister(h *Host) {
	hub.mu.Lock()
	if hub.hosts[h.ID] == h {
		delete(hub.hosts, h.ID)
	}
	hub.mu.Unlock()
	h.evict("disconnected")
}

func (hub *Hub) Lookup(hostID string) (*Host, error) {
	hub.mu.RLock()
	h := hub.hosts[hostID]
	hub.mu.RUnlock()
	if h == nil {
		return nil, ErrNoHost
	}
	return h, nil
}

// SendFunc writes one message on the host's socket; it must be safe to call
// from many goroutines.
type SendFunc func(tunnelwire.Message) error

// slots is a fixed number of places, taken without waiting.
type slots chan struct{}

func (s slots) take() bool {
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s slots) give() { <-s }

// httpSession is one request waiting on the host, or one streaming from it.
type httpSession struct {
	response chan *tunnelwire.Response
	chunks   chan *tunnelwire.BodyChunk
	pool     slots
	// ended: the host sent everything it will. canceled: the host was told to stop.
	ended, canceled bool
}

// Host is one connected host, proven by its key.
type Host struct {
	ID string

	log     *slog.Logger
	send    SendFunc
	evicted chan struct{}
	once    sync.Once
	// cancels is the host's Hello saying it reads HTTPCancel.
	cancels bool
	// strays counts unexpected messages since strayLogged; only Dispatch's
	// one goroutine touches them.
	strays      int
	strayLogged time.Time

	httpMu   sync.Mutex
	sessions map[string]*httpSession
	http     slots
	plugins  slots

	wsMu   sync.Mutex
	acks   map[string]chan *tunnelwire.WSOpenAck
	frames map[string]chan *tunnelwire.WSFrame
	closes map[string]chan *tunnelwire.WSClose
	ws     slots
}

func NewHost(id string, cancels bool, send SendFunc, log *slog.Logger) *Host {
	return &Host{
		ID: id, log: log, send: send, evicted: make(chan struct{}), cancels: cancels,
		sessions: map[string]*httpSession{},
		http:     make(slots, tunnelwire.MaxHTTP),
		plugins:  make(slots, tunnelwire.MaxPlugins),
		acks:     map[string]chan *tunnelwire.WSOpenAck{},
		frames:   map[string]chan *tunnelwire.WSFrame{},
		closes:   map[string]chan *tunnelwire.WSClose{},
		ws:       make(slots, tunnelwire.MaxWS),
	}
}

func (h *Host) evict(reason string) {
	h.once.Do(func() {
		h.log.Info("host socket closed", "host", h.ID, "reason", reason)
		close(h.evicted)
	})
}

// Dispatch hands one message from the host to the session waiting on it.
// It runs on the socket's only reader, so it never blocks.
func (h *Host) Dispatch(m tunnelwire.Message) {
	switch m := m.(type) {
	case *tunnelwire.Response:
		h.dispatchResponse(m)
	case *tunnelwire.BodyChunk:
		h.dispatchChunk(m)
	case *tunnelwire.WSOpenAck:
		h.dispatchAck(m)
	case *tunnelwire.WSFrame:
		h.dispatchFrame(m)
	case *tunnelwire.WSClose:
		h.dispatchClose(m)
	default:
		h.stray(m.Kind(), time.Now())
	}
}

// stray logs unexpected messages at most once a minute per host, with how
// many came, so a broken or hostile host can't flood the log.
func (h *Host) stray(kind tunnelwire.Kind, now time.Time) {
	h.strays++
	if now.Sub(h.strayLogged) < time.Minute {
		return
	}
	h.log.Warn("unexpected messages from host", "host", h.ID, "kind", kind, "count", h.strays)
	h.strays, h.strayLogged = 0, now
}

// ---- HTTP ----

// HTTPResult is the host's answer; Chunks is set when the body streams.
type HTTPResult struct {
	Response *tunnelwire.Response
	Chunks   <-chan *tunnelwire.BodyChunk
}

// DoHTTP sends req and waits for the host's response. After a streaming
// result the caller must call FinishHTTP.
func (h *Host) DoHTTP(ctx context.Context, req *tunnelwire.Request) (*HTTPResult, error) {
	return h.do(ctx, req.SessionID, h.http, req)
}

// DoPlugin is DoHTTP for a request to the environment's plugins.
func (h *Host) DoPlugin(ctx context.Context, req *tunnelwire.PluginRequest) (*HTTPResult, error) {
	return h.do(ctx, req.SessionID, h.plugins, req)
}

func (h *Host) do(ctx context.Context, id string, pool slots, req tunnelwire.Message) (*HTTPResult, error) {
	if !pool.take() {
		return nil, ErrHostBusy
	}
	s := &httpSession{response: make(chan *tunnelwire.Response, 1), chunks: make(chan *tunnelwire.BodyChunk, chunkBuffer), pool: pool}
	h.httpMu.Lock()
	if _, taken := h.sessions[id]; taken {
		h.httpMu.Unlock()
		pool.give()
		return nil, fmt.Errorf("duplicate session %s", id)
	}
	h.sessions[id] = s
	h.httpMu.Unlock()

	if err := h.send(req); err != nil {
		h.FinishHTTP(id)
		return nil, fmt.Errorf("send request: %w", err)
	}
	timeout := time.NewTimer(httpTimeout)
	defer timeout.Stop()
	select {
	case resp := <-s.response:
		if resp.Streaming {
			return &HTTPResult{Response: resp, Chunks: s.chunks}, nil
		}
		h.FinishHTTP(id)
		return &HTTPResult{Response: resp}, nil
	case <-ctx.Done():
		h.FinishHTTP(id)
		return nil, ctx.Err()
	case <-h.evicted:
		h.FinishHTTP(id)
		return nil, ErrHostClosed
	case <-timeout.C:
		h.FinishHTTP(id)
		return nil, fmt.Errorf("the host took longer than %s to answer", httpTimeout)
	}
}

// FinishHTTP ends a session and gives back its place. One the host hasn't
// finished, because its visitor left, waited too long or fell behind, is
// canceled on the host too. A second call is a no-op.
func (h *Host) FinishHTTP(id string) {
	h.httpMu.Lock()
	s := h.sessions[id]
	delete(h.sessions, id)
	cancel := s != nil && !s.ended && !s.canceled
	h.httpMu.Unlock()
	if s == nil {
		return
	}
	s.pool.give()
	if cancel {
		h.cancel(id)
	}
}

// cancel tells the host to stop a session, if it reads HTTPCancel. A failed
// write means the socket is gone, and the host's work with it.
func (h *Host) cancel(id string) {
	if h.cancels {
		_ = h.send(&tunnelwire.HTTPCancel{SessionID: id})
	}
}

func (h *Host) dispatchResponse(r *tunnelwire.Response) {
	h.httpMu.Lock()
	s := h.sessions[r.SessionID]
	if s != nil && !r.Streaming {
		s.ended = true
	}
	h.httpMu.Unlock()
	if s == nil {
		return
	}
	select {
	case s.response <- r:
	default:
	}
}

// dispatchChunk drops only this session when its visitor falls behind, so a
// slow download never stalls the host's other sessions; the host stops it
// at once rather than once the visitor has read what was buffered.
func (h *Host) dispatchChunk(c *tunnelwire.BodyChunk) {
	h.httpMu.Lock()
	defer h.httpMu.Unlock()
	s := h.sessions[c.SessionID]
	if s == nil || s.canceled {
		return
	}
	if len(c.Data) <= maxChunk {
		select {
		case s.chunks <- c:
			if c.EndOfStream {
				s.ended = true
			}
			return
		default:
		}
	}
	// An oversized chunk is a broken host; a full buffer, a visitor behind.
	h.log.Warn("dropping session", "host", h.ID, "session", c.SessionID, "bytes", len(c.Data))
	s.canceled = true
	close(s.chunks)
	// Off the socket's only reader, which never waits on a write.
	go h.cancel(c.SessionID)
}

// ---- WebSocket ----

// WSBridge carries one visitor WebSocket after the host opened its side.
type WSBridge struct {
	Ack    *tunnelwire.WSOpenAck
	Frames <-chan *tunnelwire.WSFrame
	Closes <-chan *tunnelwire.WSClose
}

// OpenWS asks the host to open the local socket and waits for its answer.
// The caller must call FinishWS.
func (h *Host) OpenWS(ctx context.Context, open *tunnelwire.WSOpen) (*WSBridge, error) {
	id := open.SessionID
	if !h.ws.take() {
		return nil, ErrHostBusy
	}
	ackCh := make(chan *tunnelwire.WSOpenAck, 1)
	frameCh := make(chan *tunnelwire.WSFrame, frameBuffer)
	closeCh := make(chan *tunnelwire.WSClose, 1)
	h.wsMu.Lock()
	if _, taken := h.closes[id]; taken {
		h.wsMu.Unlock()
		h.ws.give()
		return nil, fmt.Errorf("duplicate session %s", id)
	}
	h.acks[id], h.frames[id], h.closes[id] = ackCh, frameCh, closeCh
	h.wsMu.Unlock()
	defer func() {
		h.wsMu.Lock()
		delete(h.acks, id)
		h.wsMu.Unlock()
	}()

	if err := h.send(open); err != nil {
		h.FinishWS(id)
		return nil, fmt.Errorf("send ws open: %w", err)
	}
	timeout := time.NewTimer(wsOpenTimeout)
	defer timeout.Stop()
	select {
	case ack := <-ackCh:
		return &WSBridge{Ack: ack, Frames: frameCh, Closes: closeCh}, nil
	case <-ctx.Done():
		h.FinishWS(id)
		return nil, ctx.Err()
	case <-h.evicted:
		h.FinishWS(id)
		return nil, ErrHostClosed
	case <-timeout.C:
		h.FinishWS(id)
		return nil, fmt.Errorf("the host took longer than %s to open the socket", wsOpenTimeout)
	}
}

func (h *Host) SendWSFrame(f *tunnelwire.WSFrame) error { return h.send(f) }

func (h *Host) SendWSClose(c *tunnelwire.WSClose) error { return h.send(c) }

// FinishWS ends a socket's session and gives back its place; a second call is a no-op.
func (h *Host) FinishWS(id string) {
	h.wsMu.Lock()
	_, held := h.closes[id]
	delete(h.acks, id)
	delete(h.frames, id)
	delete(h.closes, id)
	h.wsMu.Unlock()
	if held {
		h.ws.give()
	}
}

func (h *Host) dispatchAck(a *tunnelwire.WSOpenAck) {
	h.wsMu.Lock()
	ch := h.acks[a.SessionID]
	h.wsMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- a:
	default:
	}
}

// dispatchFrame drops only this socket when its visitor falls behind.
func (h *Host) dispatchFrame(f *tunnelwire.WSFrame) {
	h.wsMu.Lock()
	ch := h.frames[f.SessionID]
	h.wsMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- f:
	default:
		h.log.Warn("visitor socket fell behind; dropping it", "host", h.ID, "session", f.SessionID)
		h.wsMu.Lock()
		delete(h.frames, f.SessionID)
		h.wsMu.Unlock()
		close(ch)
	}
}

func (h *Host) dispatchClose(c *tunnelwire.WSClose) {
	h.wsMu.Lock()
	ch := h.closes[c.SessionID]
	h.wsMu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- c:
	default:
	}
}
