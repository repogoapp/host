package plugins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// The plugins server is the SDK's `run.ts serve`: one Bun process for every
// project, answering one JSON object per line on a Unix socket.

const (
	startTimeout = 10 * time.Second
	dialEvery    = 50 * time.Millisecond
)

var errServerGone = errors.New("plugins: the plugins server stopped")

// wireError is the SDK's error, field errors keyed in its own camelCase.
type wireError struct {
	Code        string            `json:"code"`
	Message     string            `json:"message"`
	FieldErrors map[string]string `json:"fieldErrors,omitempty"`
}

func (e *wireError) toError() *Failure {
	if e == nil {
		return nil
	}
	return &Failure{Code: e.Code, Message: e.Message, FieldErrors: e.FieldErrors}
}

// line is anything the server sends: a reply has an id, a push a method.
type line struct {
	ID     *int            `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *wireError      `json:"error"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type reply struct {
	result json.RawMessage
	err    *wireError
}

// push is a subscription's new answer as the server sends it.
type push struct {
	Subscription string          `json:"subscription"`
	Result       json.RawMessage `json:"result"`
	Error        *wireError      `json:"error"`
}

// server is one running plugins server and the connection to it.
type server struct {
	conn net.Conn
	// stdin is held open: the server exits when it closes, so it never
	// outlives the host.
	stdin io.Closer
	log   *slog.Logger

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[int]chan reply
	nextID  int
	gone    chan struct{}
}

// startServer runs the plugins server (runtime's `serve`) under ctx and connects
// to it. onPush gets every subscription answer; onGone runs once when it ends.
func startServer(ctx context.Context, bun, runtime, socket, dataDir string, log *slog.Logger, onPush func(push), onGone func(*server)) (*server, error) {
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bun, runtime, "serve")
	cmd.Dir = filepath.Dir(runtime)
	cmd.Env = append(os.Environ(),
		"REPOGO_PLUGINS_SOCKET="+socket,
		"REPOGO_DATA_DIR="+dataDir,
		"REPOGO_PLUGINS_PARENT=stdin",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	logs := &logLines{log: log}
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugins: start the plugins server: %w", err)
	}
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	conn, err := dialWhenUp(ctx, socket, exited)
	if err != nil {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		return nil, err
	}
	s := &server{conn: conn, stdin: stdin, log: log, pending: map[int]chan reply{}, gone: make(chan struct{})}
	go s.read(onPush, onGone)
	return s, nil
}

// dialWhenUp waits for the server's socket; it appears once the server listens.
func dialWhenUp(ctx context.Context, socket string, exited <-chan struct{}) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	tick := time.NewTicker(dialEvery)
	defer tick.Stop()
	for {
		if conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket); err == nil {
			return conn, nil
		}
		select {
		case <-exited:
			return nil, errors.New("plugins: the plugins server exited while starting; see the host log")
		case <-ctx.Done():
			return nil, fmt.Errorf("plugins: the plugins server did not listen within %s", startTimeout)
		case <-tick.C:
		}
	}
}

func (s *server) read(onPush func(push), onGone func(*server)) {
	scanner := bufio.NewScanner(s.conn)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var l line
		if err := json.Unmarshal(scanner.Bytes(), &l); err != nil {
			s.log.Warn("plugins: unreadable line from the plugins server", "err", err)
			continue
		}
		if l.Method == "data" {
			var p push
			if err := json.Unmarshal(l.Params, &p); err == nil {
				onPush(p)
			}
			continue
		}
		if l.ID == nil {
			continue
		}
		s.mu.Lock()
		ch := s.pending[*l.ID]
		delete(s.pending, *l.ID)
		s.mu.Unlock()
		if ch != nil {
			ch <- reply{result: l.Result, err: l.Error}
		}
	}
	s.close()
	onGone(s)
}

// request sends one request and waits for its reply. The error is the
// connection's; a plugin's own failure comes back as the wireError.
func (s *server) request(ctx context.Context, method string, params any) (json.RawMessage, *wireError, error) {
	ch := make(chan reply, 1)
	s.mu.Lock()
	s.nextID++
	id := s.nextID
	s.pending[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}()

	b, err := json.Marshal(map[string]any{"id": id, "method": method, "params": params})
	if err != nil {
		return nil, nil, err
	}
	s.writeMu.Lock()
	_, err = s.conn.Write(append(b, '\n'))
	s.writeMu.Unlock()
	if err != nil {
		return nil, nil, errServerGone
	}
	select {
	case r := <-ch:
		return r.result, r.err, nil
	case <-s.gone:
		return nil, nil, errServerGone
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
}

func (s *server) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.gone:
		return
	default:
		close(s.gone)
	}
	_ = s.conn.Close()
	_ = s.stdin.Close()
}

// logLines passes the server's output to the host log a line at a time; plugin
// logs (ctx.log, console) arrive this way.
type logLines struct {
	log *slog.Logger
	mu  sync.Mutex
	buf []byte
}

func (l *logLines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			break
		}
		if text := string(l.buf[:i]); text != "" {
			l.log.Info("plugins server", "line", text)
		}
		l.buf = l.buf[i+1:]
	}
	return len(p), nil
}
