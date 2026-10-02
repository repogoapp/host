package agent

import (
	"testing"
	"time"
)

func TestSessionPoolLeaseReaperAndGeneration(t *testing.T) {
	var pool SessionPool[*int]
	defer pool.Close()
	session := new(int)
	closed := 0
	busy := false
	epoch := pool.Epoch()
	if !pool.Put(epoch, "chat", session, func(time.Time) bool { return !busy }, func() { closed++ }) {
		t.Fatal("put")
	}
	pool.Sweep(time.Now())
	if closed != 0 {
		t.Fatal("reaped leased session")
	}
	pool.Release("chat", session)
	busy = true
	pool.Sweep(time.Now())
	if closed != 0 {
		t.Fatal("reaped busy session")
	}
	busy = false
	pool.Sweep(time.Now())
	if closed != 1 {
		t.Fatalf("close count %d", closed)
	}
	pool.Close()
	if pool.Put(epoch, "late", new(int), func(time.Time) bool { return true }, func() {}) {
		t.Fatal("startup crossed Close")
	}
}

func TestSessionPoolRetireOnlyMatchingSession(t *testing.T) {
	var pool SessionPool[*int]
	defer pool.Close()
	a, b := new(int), new(int)
	closed := 0
	pool.Put(pool.Epoch(), "chat", a, func(time.Time) bool { return false }, func() { closed++ })
	pool.Retire("chat", b)
	if closed != 0 {
		t.Fatal("retired replacement")
	}
	pool.Retire("chat", a)
	pool.Retire("chat", a)
	if closed != 1 {
		t.Fatalf("close count %d", closed)
	}
}
