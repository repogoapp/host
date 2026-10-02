package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const maxLine = 16 << 20
const maxStderr = 32 << 10

type Exit struct {
	Code   int
	Signal string
	Err    error
	Stderr string
}

type Client struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	cancel    context.CancelFunc
	ctx       context.Context
	handlers  Handlers
	messages  chan Message
	done      chan struct{}
	exited    chan struct{}
	writes    chan struct{}
	mu        sync.Mutex
	nextID    uint64
	pending   map[string]chan controlResponse
	requests  map[string]context.CancelFunc
	exit      Exit
	stderr    []byte
	failure   error
	closeOnce sync.Once
}

func Start(ctx context.Context, opts Options, handlers Handlers) (*Client, error) {
	args, err := opts.args()
	if err != nil {
		return nil, err
	}
	procCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(procCtx, opts.Executable, args...)
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
		return nil, fmt.Errorf("start claude: %w", err)
	}
	outWriter.Close()
	errWriter.Close()
	c := &Client{cmd: cmd, stdin: stdin, cancel: cancel, ctx: procCtx, handlers: handlers,
		messages: make(chan Message, 256), done: make(chan struct{}), exited: make(chan struct{}), writes: make(chan struct{}, 1), pending: map[string]chan controlResponse{}, requests: map[string]context.CancelFunc{}}
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
		close(c.messages)
		close(c.done)
	}()
	return c, nil
}

func (c *Client) Messages() <-chan Message { return c.messages }
func (c *Client) Done() <-chan struct{}    { return c.done }
func (c *Client) Exit() Exit               { c.mu.Lock(); defer c.mu.Unlock(); return c.exit }

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

func (c *Client) readMessages(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxLine)
	for scanner.Scan() {
		line := scanner.Bytes()
		var envelope struct {
			Type      string          `json:"type"`
			RequestID string          `json:"request_id"`
			Request   json.RawMessage `json:"request"`
			Response  controlResponse `json:"response"`
		}
		if err := json.Unmarshal(line, &envelope); err != nil {
			c.fail(fmt.Errorf("claudecode: malformed stdout frame: %w", err))
			return
		}
		switch envelope.Type {
		case "":
			c.fail(errors.New("claudecode: stdout frame has no type"))
			return
		case "keep_alive":
		case "control_response":
			c.mu.Lock()
			ch := c.pending[envelope.Response.RequestID]
			delete(c.pending, envelope.Response.RequestID)
			c.mu.Unlock()
			if ch != nil {
				ch <- envelope.Response
			}
		case "control_cancel_request":
			c.mu.Lock()
			stop := c.requests[envelope.RequestID]
			c.mu.Unlock()
			if stop != nil {
				stop()
			}
		case "control_request":
			c.handleControlRequest(envelope.RequestID, envelope.Request)
		default:
			var m Message
			if err := json.Unmarshal(line, &m); err != nil {
				c.fail(fmt.Errorf("claudecode: malformed %s message: %w", envelope.Type, err))
				return
			}
			m.Raw = append(json.RawMessage(nil), line...)
			select {
			case c.messages <- m:
			default:
				c.fail(errors.New("claudecode: message buffer exhausted"))
				return
			}
		}
	}
	if err := scanner.Err(); err != nil {
		c.fail(fmt.Errorf("claudecode: stdout: %w", err))
		return
	}
	// EOF can precede Wait; give a normal exit time to retain its real status.
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-c.exited:
	case <-c.ctx.Done():
	case <-timer.C:
		c.fail(errors.New("claudecode: stdout closed while process was running"))
	}
}

func (c *Client) writeMessage(ctx context.Context, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	select {
	case c.writes <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return errors.New("claudecode: process closed")
	}
	defer func() { <-c.writes }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.ctx.Err(); err != nil {
		return errors.New("claudecode: process closed")
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
		return errors.New("claudecode: process closed")
	case <-timer.C:
		err := errors.New("claudecode: stdin write timed out")
		c.fail(err)
		return err
	}
}

func (c *Client) Send(ctx context.Context, message UserMessage) error {
	return c.writeMessage(ctx, message)
}
