package ai

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"
)

const (
	// The model's training max sequence length.
	contextLen = 2304
	// A cold start mmaps the 400 MB model and compiles the Metal kernels.
	startTimeout = 60 * time.Second
)

type serverOptions struct {
	binary, model string
	cpu           bool
}

// server is a running llama-server on a free localhost port.
type server struct {
	baseURL string
	cmd     *exec.Cmd
	done    chan struct{}
}

// startServer waits on ctx for the sidecar to be healthy, but the process
// itself belongs to the Model: it outlives the call that started it.
func startServer(ctx context.Context, o serverOptions) (*server, error) {
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	ngl := "-1" // every layer on the GPU
	if o.cpu {
		ngl = "0"
	}
	cmd := exec.Command(o.binary,
		"-m", o.model,
		"--host", "127.0.0.1", "--port", strconv.Itoa(port),
		"-c", strconv.Itoa(contextLen),
		"-ngl", ngl,
		// Qwen2.5's own template for a system+user pair, without llama-server's
		// tool-call parser, which rejects plain text it doesn't expect.
		"--chat-template", "chatml",
		"--log-disable",
	)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("ai: start llama-server: %w", err)
	}
	s := &server{baseURL: "http://127.0.0.1:" + strconv.Itoa(port), cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(s.done) }()

	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for !s.healthy(ctx) {
		select {
		case <-s.done:
			return nil, fmt.Errorf("ai: llama-server exited during startup: %v", cmd.ProcessState)
		case <-ctx.Done():
			s.close()
			return nil, fmt.Errorf("ai: llama-server not healthy: %w", ctx.Err())
		case <-tick.C:
		}
	}
	return s, nil
}

func (s *server) healthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func (s *server) alive() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

func (s *server) close() error {
	if !s.alive() {
		return nil
	}
	_ = s.cmd.Process.Signal(os.Interrupt)
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.done
	}
	return nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
