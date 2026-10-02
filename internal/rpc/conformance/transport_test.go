package conformance_test

import (
	"context"
	"testing"
	"time"

	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/rpc"
)

// block answers only once released, or once its caller is gone; entered and
// ended report each, so a test can see a call in flight and see it cancelled.
type block struct {
	entered, ended chan struct{}
	release        chan struct{}
}

func newBlock() *block {
	return &block{entered: make(chan struct{}, 8), ended: make(chan struct{}, 8), release: make(chan struct{})}
}

func (b *block) register(r *rpc.Router) {
	rpc.Add(r, "test.block", func(ctx context.Context, _ rpc.Caller, _ rpc.None) (rpc.Ack, error) {
		b.entered <- struct{}{}
		defer func() { b.ended <- struct{}{} }()
		select {
		case <-b.release:
			return rpc.OK, nil
		case <-ctx.Done():
			return rpc.Ack{}, ctx.Err()
		}
	})
	rpc.Add(r, "test.panic", func(context.Context, rpc.Caller, rpc.None) (rpc.Ack, error) {
		panic("boom")
	})
}

func wait(t *testing.T, ch chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A slow call must not hold up the next one on the same connection, on every
// transport alike.
func TestSlowCallDoesNotBlockTheNext(t *testing.T) {
	b := newBlock()
	h := newHost(t, b.register)
	for _, c := range callers(t, h) {
		t.Run(c.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				_, err := c.call(t, "test.block", nil)
				done <- err
			}()
			wait(t, b.entered, "the slow call to start")
			if _, err := c.call(t, "devices.list", nil); err != nil {
				t.Fatalf("second call while the first is running: %v", err)
			}
			b.release <- struct{}{}
			if err := <-done; err != nil {
				t.Fatalf("slow call: %v", err)
			}
			wait(t, b.ended, "the slow call to end")
		})
	}
}

// A handler's panic answers its caller with an internal error; the host and
// the connection carry on.
func TestPanicIsAnErrorNotACrash(t *testing.T) {
	h := newHost(t, newBlock().register)
	for _, c := range callers(t, h) {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.call(t, "test.panic", nil); !isCode(err, jsonrpc.CodeInternal) {
				t.Fatalf("panicking method answered %v, want internal error", err)
			}
			if _, err := c.call(t, "devices.list", nil); err != nil {
				t.Fatalf("connection unusable after a panic: %v", err)
			}
		})
	}
}

// A call's context ends when its connection does, so a phone that walked away
// does not leave work running for nobody.
func TestClosingTheConnectionCancelsItsCalls(t *testing.T) {
	b := newBlock()
	h := newHost(t, b.register)
	phone := h.pair(t)
	c := dialWS(t, h, phone)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.Call(ctx, "test.block", nil, nil)
	}()
	wait(t, b.entered, "the call to start")
	_ = c.Close()
	wait(t, b.ended, "the call to be cancelled")
}
