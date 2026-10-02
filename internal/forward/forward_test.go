package forward_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/forward"
	"github.com/repogo/host/internal/testwait"
)

// devServer stands in for whatever the user is running on localhost.
func devServer(t *testing.T, handler http.HandlerFunc) int {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return portOf(t, srv.URL)
}

func portOf(t *testing.T, rawURL string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(rawURL, "http://"))
	if err != nil {
		t.Fatalf("split %q: %v", rawURL, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port %q: %v", port, err)
	}
	return n
}

func TestFetchReturnsStatusHeadersAndBody(t *testing.T) {
	port := devServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		// Two values under one name: Set-Cookie is the case that breaks every
		// proxy that models headers as map[string]string.
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.WriteHeader(201)
		fmt.Fprintf(w, "path=%s ua=%s", r.URL.RequestURI(), r.Header.Get("User-Agent"))
	})

	resp, err := forward.NewFetcher(slog.Default()).Fetch(context.Background(), forward.Request{
		Port: port, Method: "GET", Path: "/hello?x=1",
		Headers: map[string][]string{"User-Agent": {"RepoGo"}},
	})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if resp.Status != 201 {
		t.Errorf("status = %d, want 201", resp.Status)
	}
	if got := string(resp.Body); got != "path=/hello?x=1 ua=RepoGo" {
		t.Errorf("body = %q — the query string and request headers must survive the hop", got)
	}
	if cookies := resp.Headers["Set-Cookie"]; len(cookies) != 2 {
		t.Errorf("Set-Cookie = %v, want both values", cookies)
	}
	if _, leaked := resp.Headers["Content-Length"]; leaked {
		t.Error("Content-Length was forwarded; it describes the hop, not the message")
	}
}

// A redirect belongs to the caller: it owns the address bar, and following one
// here would hand back a body from a URL nobody asked for.
func TestFetchDoesNotFollowRedirects(t *testing.T) {
	port := devServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		fmt.Fprint(w, "arrived")
	})

	resp, err := forward.NewFetcher(slog.Default()).Fetch(context.Background(),
		forward.Request{Port: port, Path: "/"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if resp.Status != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.Status)
	}
	if got := resp.Headers["Location"]; len(got) != 1 || got[0] != "/elsewhere" {
		t.Errorf("Location = %v", got)
	}
}

func TestFetchTruncatesAnOversizedBody(t *testing.T) {
	port := devServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Write(make([]byte, forward.MaxBodyBytes+4096))
	})

	resp, err := forward.NewFetcher(slog.Default()).Fetch(context.Background(),
		forward.Request{Port: port, Path: "/big"})
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	// Reported, not silently cut: a half-delivered asset that claims success is
	// worse than an error, because the failure surfaces somewhere else entirely.
	if !resp.Truncated {
		t.Error("an oversized body was not reported as truncated")
	}
	if len(resp.Body) != forward.MaxBodyBytes {
		t.Errorf("body = %d bytes, want the cap %d", len(resp.Body), forward.MaxBodyBytes)
	}
}

func TestFetchRejectsAnImpossiblePort(t *testing.T) {
	_, err := forward.NewFetcher(slog.Default()).Fetch(context.Background(),
		forward.Request{Port: 0, Path: "/"})
	if !errors.Is(err, forward.ErrPort) {
		t.Errorf("port 0: %v, want ErrPort", err)
	}
}

// collector stands in for the push lane.
type collector struct {
	mu     sync.Mutex
	data   []byte
	closed string
	fail   bool
}

// Send decodes what a client would: the events as JSON off the wire.
func (c *collector) Send(_ device.ID, method string, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fail {
		return errors.New("client unreachable")
	}
	switch method {
	case "forward.pipe_data":
		var ev forward.Data
		_ = json.Unmarshal(payload, &ev)
		c.data = append(c.data, ev.Data...)
	case "forward.pipe_closed":
		var ev forward.Closed
		_ = json.Unmarshal(payload, &ev)
		c.closed = ev.Reason
	}
	return nil
}

// emitter wraps the collector as what NewPipes takes.
func (c *collector) emitter() *emit.Emitter {
	return emit.New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func (c *collector) snapshot() (string, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return string(c.data), c.closed
}

// echoServer is the simplest thing with the shape that matters: it keeps
// talking after the request, which is exactly what fetch cannot carry.
func echoServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 4096)
				for {
					n, err := conn.Read(buf)
					if n > 0 {
						conn.Write(append([]byte("echo:"), buf[:n]...))
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestPipeRelaysBothDirections(t *testing.T) {
	port := echoServer(t)
	c := &collector{}
	pipes := forward.NewPipes(c.emitter(), slog.Default())

	const id = "pipe-a"
	if err := pipes.Open("phone", id, port, []byte("GET /ws HTTP/1.1\r\n\r\n")); err != nil {
		t.Fatalf("open: %v", err)
	}
	testwait.For(t, "the initial bytes to come back", func() bool {
		got, _ := c.snapshot()
		return strings.Contains(got, "GET /ws")
	})

	// The half that fetch cannot do: the server talks again, later, unprompted.
	if err := pipes.Send("phone", id, []byte("ping")); err != nil {
		t.Fatalf("send: %v", err)
	}
	testwait.For(t, "the second message", func() bool {
		got, _ := c.snapshot()
		return strings.Contains(got, "echo:ping")
	})

	pipes.Close("phone", id)
	testwait.For(t, "the close notification", func() bool {
		_, closed := c.snapshot()
		return closed != ""
	})
}

// The client names its own pipe, so the host must refuse a name already in use
// rather than quietly rebinding it and stranding the first stream.
func TestPipeRejectsADuplicateID(t *testing.T) {
	port := echoServer(t)
	pipes := forward.NewPipes((&collector{}).emitter(), slog.Default())

	if err := pipes.Open("phone", "same", port, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := pipes.Open("phone", "same", port, nil); !errors.Is(err, forward.ErrPipeExists) {
		t.Errorf("second open with the same id: %v, want ErrPipeExists", err)
	}
}

// A second paired device must not be able to write into someone else's stream
// by naming it — and since the client picks the name now, guessing is easier.
func TestPipeIsScopedToItsCaller(t *testing.T) {
	port := echoServer(t)
	pipes := forward.NewPipes((&collector{}).emitter(), slog.Default())

	const id = "pipe-a"
	if err := pipes.Open("phone", id, port, nil); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := pipes.Send("laptop", id, []byte("hello")); !errors.Is(err, forward.ErrPipeNotFound) {
		t.Errorf("another device wrote into the pipe: %v", err)
	}
	pipes.Close("laptop", id)
	if err := pipes.Send("phone", id, []byte("hello")); err != nil {
		t.Errorf("another device closed the pipe: %v", err)
	}
	// Closing a pipe that is already gone is success.
	pipes.Close("phone", id)
	pipes.Close("phone", id)
}

// A phone that walks out of coverage never says so. Without this the socket and
// its goroutine live until the process restarts.
func TestPipeClosesWhenTheClientIsUnreachable(t *testing.T) {
	port := echoServer(t)
	c := &collector{fail: true}
	pipes := forward.NewPipes(c.emitter(), slog.Default())

	if err := pipes.Open("phone", "pipe-a", port, []byte("hello")); err != nil {
		t.Fatalf("open: %v", err)
	}
	// The echo comes back, the push fails, and the pipe tears itself down
	// rather than pumping into nowhere forever.
	testwait.For(t, "the pipe to give up", func() bool {
		return pipes.Send("phone", "pipe-a", []byte("x")) != nil
	})
}
