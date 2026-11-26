package tunnelgateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/tunnelwire"
)

// recorder is a host's socket that keeps what the gateway sent it.
type recorder chan tunnelwire.Message

func (r recorder) send(m tunnelwire.Message) error {
	r <- m
	return nil
}

func (r recorder) next(t *testing.T) tunnelwire.Message {
	t.Helper()
	select {
	case m := <-r:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("nothing sent to the host")
		return nil
	}
}

// nothing checks that no message is waiting; callers wait for the code that
// would have sent one to return first.
func (r recorder) nothing(t *testing.T) {
	t.Helper()
	select {
	case m := <-r:
		t.Fatalf("sent %T %+v", m, m)
	default:
	}
}

func newTestHost(cancels bool) (*Host, recorder) {
	sent := make(recorder, 2*chunkBuffer)
	return NewHost("h1", cancels, sent.send, slog.New(slog.NewTextHandler(io.Discard, nil))), sent
}

type result struct {
	res *HTTPResult
	err error
}

func start(ctx context.Context, h *Host, id string) <-chan result {
	out := make(chan result, 1)
	go func() {
		res, err := h.DoHTTP(ctx, &tunnelwire.Request{SessionID: id})
		out <- result{res, err}
	}()
	return out
}

func wait(t *testing.T, ch <-chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("DoHTTP never returned")
		return result{}
	}
}

// A visitor who leaves before the answer cancels the host's work and frees its place.
func TestLeavingVisitorCancelsTheHost(t *testing.T) {
	h, sent := newTestHost(true)
	ctx, cancel := context.WithCancel(t.Context())
	done := start(ctx, h, "s1")
	if _, ok := sent.next(t).(*tunnelwire.Request); !ok {
		t.Fatal("request not sent")
	}
	cancel()
	if r := wait(t, done); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("err = %v", r.err)
	}
	if c, ok := sent.next(t).(*tunnelwire.HTTPCancel); !ok || c.SessionID != "s1" {
		t.Fatalf("sent %+v, want HTTPCancel s1", c)
	}
	if n := len(h.http); n != 0 {
		t.Fatalf("%d places still taken", n)
	}
}

// A host whose Hello didn't offer HTTPCancel is never sent one.
func TestNoCancelToAHostThatCannotReadIt(t *testing.T) {
	h, sent := newTestHost(false)
	ctx, cancel := context.WithCancel(t.Context())
	done := start(ctx, h, "s1")
	sent.next(t)
	cancel()
	wait(t, done)
	sent.nothing(t)
	if n := len(h.http); n != 0 {
		t.Fatalf("%d places still taken", n)
	}
}

// A whole answer, or a stream that reached its end, needs no cancel.
func TestFinishedSessionsAreNotCanceled(t *testing.T) {
	h, sent := newTestHost(true)
	done := start(t.Context(), h, "whole")
	sent.next(t)
	h.Dispatch(&tunnelwire.Response{SessionID: "whole", Status: 200})
	if r := wait(t, done); r.err != nil {
		t.Fatal(r.err)
	}

	done = start(t.Context(), h, "stream")
	sent.next(t)
	h.Dispatch(&tunnelwire.Response{SessionID: "stream", Status: 200, Streaming: true})
	r := wait(t, done)
	h.Dispatch(&tunnelwire.BodyChunk{SessionID: "stream", EndOfStream: true})
	<-r.res.Chunks
	h.FinishHTTP("stream")
	h.FinishHTTP("stream")
	sent.nothing(t)
	if n := len(h.http); n != 0 {
		t.Fatalf("%d places still taken", n)
	}
}

// A stream its visitor stopped reading before the end is canceled once.
func TestAbandonedStreamIsCanceled(t *testing.T) {
	h, sent := newTestHost(true)
	done := start(t.Context(), h, "s1")
	sent.next(t)
	h.Dispatch(&tunnelwire.Response{SessionID: "s1", Status: 200, Streaming: true})
	wait(t, done)
	h.Dispatch(&tunnelwire.BodyChunk{SessionID: "s1", Data: []byte("data: 1\n\n")})
	h.FinishHTTP("s1")
	h.FinishHTTP("s1")
	if c, ok := sent.next(t).(*tunnelwire.HTTPCancel); !ok || c.SessionID != "s1" {
		t.Fatalf("sent %+v, want HTTPCancel s1", c)
	}
	sent.nothing(t)
}

// A visitor that falls behind gets its host stopped at once, not once it has
// read what was buffered, and the visitor's finish sends no second cancel.
func TestSlowVisitorIsCanceledAtOnce(t *testing.T) {
	h, sent := newTestHost(true)
	done := start(t.Context(), h, "s1")
	sent.next(t)
	h.Dispatch(&tunnelwire.Response{SessionID: "s1", Status: 200, Streaming: true})
	r := wait(t, done)
	for range chunkBuffer + 1 {
		h.Dispatch(&tunnelwire.BodyChunk{SessionID: "s1", Data: []byte("x")})
	}
	if c, ok := sent.next(t).(*tunnelwire.HTTPCancel); !ok || c.SessionID != "s1" {
		t.Fatalf("sent %+v, want HTTPCancel s1", c)
	}
	for range r.res.Chunks {
	}
	h.FinishHTTP("s1")
	sent.nothing(t)
	if n := len(h.http); n != 0 {
		t.Fatalf("%d places still taken", n)
	}
}

// A host at its limit is refused before anything crosses its socket.
func TestBusyHostIsRefused(t *testing.T) {
	h, sent := newTestHost(true)
	for range cap(h.http) {
		h.http.take()
	}
	if r := wait(t, start(t.Context(), h, "s1")); !errors.Is(r.err, ErrHostBusy) {
		t.Fatalf("err = %v, want ErrHostBusy", r.err)
	}
	for range cap(h.ws) {
		h.ws.take()
	}
	if _, err := h.OpenWS(t.Context(), &tunnelwire.WSOpen{SessionID: "w1"}); !errors.Is(err, ErrHostBusy) {
		t.Fatalf("err = %v, want ErrHostBusy", err)
	}
	sent.nothing(t)
}

// A WebSocket's place is given back once, however often it's finished.
func TestWebSocketPlaceIsGivenBackOnce(t *testing.T) {
	h, sent := newTestHost(true)
	opened := make(chan error, 1)
	go func() {
		_, err := h.OpenWS(t.Context(), &tunnelwire.WSOpen{SessionID: "w1"})
		opened <- err
	}()
	sent.next(t)
	h.Dispatch(&tunnelwire.WSOpenAck{SessionID: "w1"})
	if err := <-opened; err != nil {
		t.Fatal(err)
	}
	h.ws.take()
	h.FinishWS("w1")
	h.FinishWS("w1")
	if n := len(h.ws); n != 1 {
		t.Fatalf("%d places taken, want only the one this test holds", n)
	}
}

// countingHandler counts log records.
type countingHandler struct {
	slog.Handler
	n *int
}

func (h countingHandler) Handle(_ context.Context, _ slog.Record) error { *h.n++; return nil }

// A host sending junk writes one log line a minute, not one per message.
func TestStrayMessagesLogOnceAMinute(t *testing.T) {
	var n int
	h := NewHost("h1", false, func(tunnelwire.Message) error { return nil }, slog.New(countingHandler{slog.NewTextHandler(io.Discard, nil), &n}))
	now := time.Now()
	for range 1000 {
		h.stray(tunnelwire.KindReady, now)
	}
	h.stray(tunnelwire.KindReady, now.Add(time.Minute))
	if n != 2 {
		t.Fatalf("%d log lines, want 2", n)
	}
}

// An unexpected error's text stays in the log.
func TestUnexpectedErrorsAreNotShown(t *testing.T) {
	s := &Server{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, _, msg := s.hostError(errors.New("dial tcp 172.16.0.5:8080: refused"))
	if strings.Contains(msg, "172.16") {
		t.Fatalf("shown %q", msg)
	}
}
