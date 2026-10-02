// Package stdiorpc owns a child process speaking newline-delimited JSON-RPC.
package stdiorpc

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"
)

const maxLine = 16 << 20
const maxStderr = 32 << 10

// Streaming deltas arrive faster than a turn's consumer can fall behind by;
// a full buffer means the consumer is gone, not busy.
const notificationBuffer = 4096

// Exit is how the process ended.
type Exit struct {
	Code   int
	Signal string
	Err    error
	Stderr string
}

// Error is a JSON-RPC error the server answered a call with.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("stdiorpc: %s (%d)", e.Message, e.Code) }

// Handlers answer the requests the server sends. Request's ctx ends when the server
// resolves the request itself (a turn interrupted) or the process exits.
type Handlers struct {
	Request func(ctx context.Context, r Request) (any, error)
	// Notification runs in read order before the next reply is delivered; it must not call back into Client.
	Notification func(Notification) error
	Resolved     func(string, json.RawMessage) json.RawMessage
}

type response struct {
	result json.RawMessage
	err    *Error
}

// Client is one running JSON-RPC process.
type Client struct {
	cmd           *exec.Cmd
	stdin         io.WriteCloser
	cancel        context.CancelFunc
	ctx           context.Context
	handlers      Handlers
	version       string
	notifications chan Notification
	done          chan struct{}
	exited        chan struct{}
	writes        chan struct{}
	mu            sync.Mutex
	nextID        int64
	pending       map[int64]chan response
	requests      map[string]context.CancelFunc
	exit          Exit
	stderr        []byte
	failure       error
	closeOnce     sync.Once
}

// Start runs the server in its own process group, so closing it also ends
// the commands and tools it started.
func Start(ctx context.Context, opts Options, handlers Handlers) (*Client, error) {
	if opts.Executable == "" || opts.Cwd == "" {
		return nil, errors.New("stdiorpc: executable and cwd required")
	}
	procCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(procCtx, opts.Executable, opts.Args...)
	cmd.Dir, cmd.Env = opts.Cwd, opts.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, outWriter, err := os.Pipe()
	if err != nil {
		cancel()
		stdin.Close()
		return nil, err
	}
	stderr, errWriter, err := os.Pipe()
	if err != nil {
		cancel()
		stdin.Close()
		stdout.Close()
		outWriter.Close()
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = outWriter, errWriter
	if err = cmd.Start(); err != nil {
		cancel()
		stdin.Close()
		stdout.Close()
		outWriter.Close()
		stderr.Close()
		errWriter.Close()
		return nil, fmt.Errorf("start JSON-RPC process: %w", err)
	}
	outWriter.Close()
	errWriter.Close()
	c := &Client{cmd: cmd, stdin: stdin, cancel: cancel, ctx: procCtx, handlers: handlers, version: opts.Version,
		notifications: make(chan Notification, notificationBuffer), done: make(chan struct{}),
		exited: make(chan struct{}), writes: make(chan struct{}, 1),
		pending: map[int64]chan response{}, requests: map[string]context.CancelFunc{}}
	var drains sync.WaitGroup
	drains.Add(2)
	go func() { defer drains.Done(); defer stdout.Close(); c.readMessages(stdout) }()
	go func() { defer drains.Done(); defer stderr.Close(); c.readStderr(stderr) }()
	go func() {
		err := cmd.Wait()
		close(c.exited)
		// Descendants must not keep the pipes or tools alive after their owner exits.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		drains.Wait()
		c.mu.Lock()
		c.exit = Exit{Code: cmd.ProcessState.ExitCode(), Err: err, Stderr: string(c.stderr)}
		if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			c.exit.Signal = status.Signal().String()
		}
		if c.failure != nil {
			c.exit.Err = c.failure
		}
		for _, stop := range c.requests {
			stop()
		}
		c.mu.Unlock()
		cancel()
		close(c.notifications)
		close(c.done)
	}()
	return c, nil
}

// Notifications closes when the process has exited and every one is read.
func (c *Client) Notifications() <-chan Notification { return c.notifications }
func (c *Client) Done() <-chan struct{}              { return c.done }
func (c *Client) Exit() Exit                         { c.mu.Lock(); defer c.mu.Unlock(); return c.exit }

// Stderr is the tail of what the process has written there so far.
func (c *Client) Stderr() string { c.mu.Lock(); defer c.mu.Unlock(); return string(c.stderr) }

func (c *Client) Close() error {
	c.closeOnce.Do(func() { c.cancel(); _ = c.stdin.Close() })
	<-c.done
	return nil
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.failure == nil {
		c.failure = err
	}
	c.mu.Unlock()
	c.cancel()
}

func (c *Client) readStderr(r io.Reader) {
	b := make([]byte, 4096)
	for {
		n, err := r.Read(b)
		if n > 0 {
			c.mu.Lock()
			c.stderr = append(c.stderr, b[:n]...)
			if len(c.stderr) > maxStderr {
				c.stderr = append([]byte(nil), c.stderr[len(c.stderr)-maxStderr:]...)
			}
			c.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// envelope is any JSON-RPC message: a request has an id and a method, a
// notification only a method, and a response an id with a result or error.
type envelope struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

func (c *Client) readMessages(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxLine)
	for scanner.Scan() {
		var m envelope
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			c.fail(fmt.Errorf("stdiorpc: malformed stdout frame: %w", err))
			return
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			c.handleRequest(Request{ID: m.ID, Method: m.Method, Params: m.Params})
		case m.Method != "":
			if c.handlers.Resolved != nil {
				if id := c.handlers.Resolved(m.Method, m.Params); len(id) > 0 {
					c.resolved(id)
				}
			}
			if c.handlers.Notification != nil {
				if err := c.handlers.Notification(Notification{Method: m.Method, Params: m.Params}); err != nil {
					c.fail(err)
					return
				}
				continue
			}
			select {
			case c.notifications <- Notification{Method: m.Method, Params: m.Params}:
			default:
				c.fail(errors.New("stdiorpc: notification buffer exhausted"))
				return
			}
		case len(m.ID) > 0:
			c.answer(m)
		default:
			c.fail(errors.New("stdiorpc: stdout frame is neither a message nor a reply"))
			return
		}
	}
	if err := scanner.Err(); err != nil {
		c.fail(fmt.Errorf("stdiorpc: stdout: %w", err))
		return
	}
	// EOF can precede Wait; give a normal exit time to retain its real status.
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-c.exited:
	case <-c.ctx.Done():
	case <-timer.C:
		c.fail(errors.New("stdiorpc: stdout closed while process was running"))
	}
}

func (c *Client) answer(m envelope) {
	id, err := strconv.ParseInt(string(m.ID), 10, 64)
	if err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch != nil {
		ch <- response{result: m.Result, err: m.Error}
	}
}

// resolved releases a request the server has settled itself, so a person is no
// longer asked something nobody is waiting on.
func (c *Client) resolved(id json.RawMessage) {
	c.mu.Lock()
	stop := c.requests[requestKey(id)]
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// requestKey names an id however the server wrote it: a number or a string.
func requestKey(id json.RawMessage) string { return string(bytes.TrimSpace(id)) }

func (c *Client) handleRequest(r Request) {
	key := requestKey(r.ID)
	ctx, cancel := context.WithCancel(c.ctx)
	c.mu.Lock()
	if len(c.requests) >= 1024 {
		c.mu.Unlock()
		cancel()
		c.fail(errors.New("stdiorpc: too many pending inbound requests"))
		return
	}
	if _, exists := c.requests[key]; exists {
		c.mu.Unlock()
		cancel()
		c.fail(errors.New("stdiorpc: duplicate inbound request id"))
		return
	}
	c.requests[key] = cancel
	c.mu.Unlock()
	go func() {
		defer cancel()
		defer func() { c.mu.Lock(); delete(c.requests, key); c.mu.Unlock() }()
		result, err := c.dispatch(ctx, r)
		// Resolved by the server or the process is gone: nobody reads a reply.
		if ctx.Err() != nil {
			return
		}
		reply := map[string]any{"id": r.ID}
		if err != nil {
			var rpcError *Error
			if errors.As(err, &rpcError) {
				reply["error"] = rpcError
			} else {
				reply["error"] = Error{Code: -32000, Message: err.Error()}
			}
		} else {
			reply["result"] = result
		}
		if err := c.write(c.ctx, reply); err != nil {
			c.fail(err)
		}
	}()
}

func (c *Client) dispatch(ctx context.Context, r Request) (any, error) {
	if c.handlers.Request == nil {
		return nil, &Error{Code: -32601, Message: "unsupported request: " + r.Method}
	}
	return c.handlers.Request(ctx, r)
}

// Call sends one request and decodes its result into out, which may be nil.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	if params == nil {
		params = struct{}{}
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan response, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.write(ctx, map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		exit := c.Exit()
		return fmt.Errorf("stdiorpc: process exited (%d) before answering %s: %v", exit.Code, method, exit.Err)
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if out != nil && len(r.result) > 0 {
			return json.Unmarshal(r.result, out)
		}
		return nil
	}
}

// Notify sends a notification, which the server does not answer.
func (c *Client) Notify(ctx context.Context, method string, params any) error {
	if params == nil {
		params = struct{}{}
	}
	return c.write(ctx, map[string]any{"method": method, "params": params})
}

func (c *Client) write(ctx context.Context, value map[string]any) error {
	if c.version != "" {
		value["jsonrpc"] = c.version
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	select {
	case c.writes <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return errors.New("stdiorpc: process closed")
	}
	defer func() { <-c.writes }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.ctx.Err(); err != nil {
		return errors.New("stdiorpc: process closed")
	}
	done := make(chan error, 1)
	go func() { _, err := c.stdin.Write(append(b, '\n')); done <- err }()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		c.fail(ctx.Err())
		return ctx.Err()
	case <-c.ctx.Done():
		return errors.New("stdiorpc: process closed")
	case <-timer.C:
		err := errors.New("stdiorpc: stdin write timed out")
		c.fail(err)
		return err
	}
}
