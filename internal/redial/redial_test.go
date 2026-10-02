package redial

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Waits grow from about Min and never pass Max, however many attempts fail.
func TestBackoffStaysWithinItsBounds(t *testing.T) {
	var b backoff
	for i := range 20 {
		w := b.next()
		if w < Min/2 || w > Max {
			t.Fatalf("attempt %d waits %v, want within [%v, %v]", i, w, Min/2, Max)
		}
		if i == 0 && w > Min {
			t.Fatalf("first wait %v, want at most %v", w, Min)
		}
	}
	if b.d != Max {
		t.Fatalf("backoff settled at %v, want %v", b.d, Max)
	}
}

// Run hands every end to retry and stops when ctx does.
func TestRunRetriesUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	failed := errors.New("dial failed")
	var got error
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, func(context.Context) error { return failed }, func(err error, wait time.Duration) {
			got = err
			cancel()
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop when its context ended")
	}
	if !errors.Is(got, failed) {
		t.Fatalf("retry heard %v, want the session's error", got)
	}
}
