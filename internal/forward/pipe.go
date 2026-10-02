package forward

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/errkind"
)

// The pipe lane: a raw TCP relay between a client and a loopback dev server,
// carrying opaque bytes so WebSocket upgrades and SSE flow through unframed.
// The client names the pipe so it can register before the first push arrives.

const (
	// A pipe with nothing on it is a tab the user closed, or a phone that
	// walked out of coverage. Neither will ever say so.
	pipeIdleTimeout = 5 * time.Minute

	// Bounds one buffer, not a read: small enough that a chatty stream is not one
	// message, large enough that an HMR payload is not a hundred.
	pipeReadBuffer = 32 * 1024

	// Per host, across all clients. A pipe is a file descriptor and a goroutine;
	// unbounded, a misbehaving client exhausts both.
	maxOpenPipes = 64
)

var (
	ErrPipeNotFound = errkind.New(errkind.NotFound, "forward: no such pipe")
	ErrPipeLimit    = errkind.New(errkind.Unavailable, "forward: too many open pipes")
	ErrPipeExists   = errkind.New(errkind.Invalid, "forward: pipe id is already open")
	ErrPipeID       = errkind.New(errkind.Invalid, "forward: pipe id is required")
)

// Pipes owns every open relay on this host.
type Pipes struct {
	// log records why a pipe died. Opens alone cannot distinguish a stream that
	// is working from one reconnecting every second.
	log *slog.Logger

	// emit is how a pipe reports bytes and closure to its client.
	emit *emit.Emitter

	mu    sync.Mutex
	pipes map[string]*pipe
}

type pipe struct {
	id     string
	caller device.ID
	conn   net.Conn

	// Byte counters, reported on close. A pipe that carried nothing in either
	// direction failed differently from one that carried a handshake and then
	// stopped, and the close reason alone does not tell them apart.
	fromServer int
	toServer   int

	closeOnce sync.Once
}

func NewPipes(emit *emit.Emitter, log *slog.Logger) *Pipes {
	return &Pipes{emit: emit, log: log, pipes: map[string]*pipe{}}
}

// Open dials the dev server, writes the caller's raw HTTP request verbatim, and
// pumps the response back; nothing here has to agree on what an upgrade looks
// like.
func (p *Pipes) Open(caller device.ID, id string, port int, initial []byte) error {
	if err := p.open(caller, id, port, initial); err != nil {
		p.log.Warn("forward: pipe refused", "port", port, "pipe", id, "err", err)
		return err
	}
	p.log.Info("forward: pipe open", "port", port, "pipe", id, "bytes", len(initial))
	return nil
}

func (p *Pipes) open(caller device.ID, id string, port int, initial []byte) error {
	if id == "" {
		return ErrPipeID
	}
	if port <= 0 || port > 65535 {
		return ErrPort
	}

	p.mu.Lock()
	if len(p.pipes) >= maxOpenPipes {
		p.mu.Unlock()
		return ErrPipeLimit
	}
	if _, taken := p.pipes[id]; taken {
		p.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrPipeExists, id)
	}
	// Reserved before the dial, so two opens racing on one id cannot both win.
	pi := &pipe{id: id, caller: caller}
	p.pipes[id] = pi
	p.mu.Unlock()

	conn, err := net.DialTimeout("tcp4", "127.0.0.1:"+strconv.Itoa(port), 10*time.Second)
	if err != nil {
		p.forget(id)
		return fmt.Errorf("forward: dial 127.0.0.1:%d: %w", port, err)
	}
	pi.conn = conn

	// The request line only: headers carry cookies.
	p.log.Info("forward: pipe request", "pipe", id, "line", firstLine(initial))

	if len(initial) > 0 {
		if _, err := conn.Write(initial); err != nil {
			conn.Close()
			p.forget(id)
			return fmt.Errorf("forward: write upgrade request: %w", err)
		}
	}

	go p.pump(pi)
	return nil
}

func (p *Pipes) forget(id string) {
	p.mu.Lock()
	delete(p.pipes, id)
	p.mu.Unlock()
}

// Send relays client → server bytes.
func (p *Pipes) Send(caller device.ID, id string, data []byte) error {
	pi, err := p.lookup(caller, id)
	if err != nil {
		return err
	}
	_ = pi.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	pi.toServer += len(data)
	if _, err := pi.conn.Write(data); err != nil {
		p.closePipe(pi, err.Error())
		return fmt.Errorf("forward: write: %w", err)
	}
	return nil
}

// Close ends the caller's pipe. One already gone is closed: the client's
// close and the server's hang-up race constantly.
func (p *Pipes) Close(caller device.ID, id string) {
	if pi, err := p.lookup(caller, id); err == nil {
		p.closePipe(pi, "closed by client")
	}
}

func (p *Pipes) lookup(caller device.ID, id string) (*pipe, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pi, ok := p.pipes[id]
	// Scoped to the caller, not just the id: ids are small integers, and a
	// second paired device must not be able to write into someone else's stream
	// by guessing one.
	if !ok || pi.caller != caller {
		return nil, fmt.Errorf("%w: %s", ErrPipeNotFound, id)
	}
	return pi, nil
}

// pump relays server → client until the connection ends.
func (p *Pipes) pump(pi *pipe) {
	buf := make([]byte, pipeReadBuffer)
	for {
		_ = pi.conn.SetReadDeadline(time.Now().Add(pipeIdleTimeout))
		n, err := pi.conn.Read(buf)
		if n > 0 {
			if pi.fromServer == 0 {
				// The status line says whether the upgrade was accepted at all.
				p.log.Info("forward: pipe reply", "pipe", pi.id, "line", firstLine(buf[:n]))
			}
			pi.fromServer += n
			if sendErr := p.emit.To(pi.caller, Data{PipeID: pi.id, Data: buf[:n]}); sendErr != nil {
				// The client is gone or the link is down. Nothing will read
				// this stream again, so holding the socket open serves no one.
				p.closePipe(pi, "client unreachable")
				return
			}
		}
		if err != nil {
			reason := "closed by server"
			if !errors.Is(err, io.EOF) {
				reason = err.Error()
			}
			p.closePipe(pi, reason)
			return
		}
	}
}

// firstLine is the first CRLF-delimited line, capped. Used for logging only.
func firstLine(b []byte) string {
	const cap = 120
	if i := bytes.IndexByte(b, '\r'); i >= 0 && i < cap {
		return string(b[:i])
	}
	if len(b) > cap {
		return string(b[:cap])
	}
	return string(b)
}

// closePipe is idempotent: the read pump, an explicit close, and a client
// disconnect all race to get here, and only one of them may announce it.
func (p *Pipes) closePipe(pi *pipe, reason string) {
	pi.closeOnce.Do(func() {
		p.mu.Lock()
		delete(p.pipes, pi.id)
		p.mu.Unlock()

		p.log.Info("forward: pipe closed", "pipe", pi.id, "reason", reason,
			"from_server", pi.fromServer, "from_client", pi.toServer)
		if pi.conn != nil {
			pi.conn.Close()
		}
		_ = p.emit.To(pi.caller, Closed{PipeID: pi.id, Reason: reason})
	})
}
