package hostclient

import (
	"errors"
	"testing"
	"time"

	"github.com/repogo/host/internal/jsonrpc"
)

// A call that already has its reply when the connection drops must not wedge
// the reader, which holds the lock every other call needs.
func TestFailDoesNotBlockOnAnsweredCall(t *testing.T) {
	answered := make(chan *jsonrpc.Message, 1)
	answered <- &jsonrpc.Message{}
	c := &Client{pending: map[string]chan *jsonrpc.Message{"1": answered}}

	done := make(chan struct{})
	go func() {
		c.fail(errors.New("gone"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fail blocked on a waiter that already had its reply")
	}
}
