// Package idempotency runs a request a caller may send again once: a phone
// that lost the reply resends with the same key, and the repeat gets the
// first answer instead of running twice. Answers are kept a day, so a
// resend from the outbox or after a host restart is caught too.
//
// It is best effort, not exactly once: a request runs before its answer is
// saved, so a crash between the two lets a repeat run it again. A failed
// request isn't kept, so it can be fixed and sent again; use it only for
// requests that fail before they change anything.
package idempotency

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/wirejson"
)

// Keep is how long an answer is kept.
const Keep = 24 * time.Hour

// pruneEvery spaces out the deletes of old answers, which run on a save.
const pruneEvery = time.Minute

// ErrReused is a key sent again with a different request.
var ErrReused = errkind.New(errkind.Invalid, "this idempotency key was used for a different request")

// errUnfinished answers a repeat whose first request never finished (it
// panicked, or its answer couldn't be encoded).
var errUnfinished = errors.New("the first request with this idempotency key didn't finish")

// Store keeps the answers: *store.Store.
type Store interface {
	IdempotencyKey(scope, method, key string) (hash, result []byte, ok bool, err error)
	SaveIdempotencyKey(scope, method, key string, hash, result []byte, createdAt int64) error
	DeleteIdempotencyKey(scope, method, key string) error
	PruneIdempotency(before int64) error
}

// Keys runs keyed requests once. The zero value is not usable; use New.
type Keys struct {
	store Store
	now   func() time.Time

	mu      sync.Mutex
	running map[id]*call
	pruned  time.Time
}

type id struct{ scope, method, key string }

// call is a request still running, which a repeat waits on.
type call struct {
	done   chan struct{}
	hash   []byte
	result []byte
	err    error
	// waiters counts the repeats waiting on it, under Keys.mu.
	waiters int
}

// New prunes answers older than a day and returns Keys over s.
func New(s Store) *Keys {
	k := &Keys{store: s, now: time.Now, running: map[id]*call{}}
	k.mu.Lock()
	k.pruneLocked()
	k.mu.Unlock()
	return k
}

// Do runs run once for (scope, method, key) and hands every repeat the
// first answer, with replayed true. request is what the caller sent, its
// key cleared: a repeat whose request differs gets ErrReused. A repeat
// arriving while the first still runs waits for it, until ctx ends. An
// empty scope or key always runs.
func Do[T any](ctx context.Context, k *Keys, scope, method, key string, request any, run func() (T, error)) (out T, replayed bool, err error) {
	if k == nil || scope == "" || key == "" {
		out, err = run()
		return out, false, err
	}
	hash, err := hashOf(request)
	if err != nil {
		return out, false, err
	}
	at := id{scope, method, key}

	k.mu.Lock()
	if c, ok := k.running[at]; ok {
		c.waiters++
		k.mu.Unlock()
		select {
		case <-c.done:
		case <-ctx.Done():
			return out, false, ctx.Err()
		}
		if !bytes.Equal(c.hash, hash) {
			return out, false, ErrReused
		}
		if c.err != nil {
			return out, false, c.err
		}
		return decode[T](c.result)
	}
	saved, result, ok, err := k.store.IdempotencyKey(scope, method, key)
	if err != nil {
		k.mu.Unlock()
		return out, false, err
	}
	if ok {
		k.mu.Unlock()
		if !bytes.Equal(saved, hash) {
			return out, false, ErrReused
		}
		return decode[T](result)
	}
	c := &call{done: make(chan struct{}), hash: hash, err: errUnfinished}
	k.running[at] = c
	k.mu.Unlock()
	// Released after the answer is saved, and on a panic too, so no repeat
	// waits forever and none runs the request again in between.
	defer func() {
		k.mu.Lock()
		delete(k.running, at)
		k.mu.Unlock()
		close(c.done)
	}()

	out, err = run()
	if err != nil {
		// Not kept: the request failed before changing anything.
		c.err = err
		return out, false, err
	}
	b, err := wirejson.Marshal(out)
	if err != nil {
		return out, false, nil
	}
	// A failed save loses only the dedup of a later repeat; the request ran.
	_ = k.store.SaveIdempotencyKey(scope, method, key, hash, b, k.now().UnixMilli())
	c.result, c.err = b, nil
	k.mu.Lock()
	k.pruneLocked()
	k.mu.Unlock()
	return out, false, nil
}

// Forget drops a kept answer, so the next request with its key runs anew.
func (k *Keys) Forget(scope, method, key string) error {
	return k.store.DeleteIdempotencyKey(scope, method, key)
}

func (k *Keys) pruneLocked() {
	now := k.now()
	if now.Sub(k.pruned) < pruneEvery {
		return
	}
	k.pruned = now
	_ = k.store.PruneIdempotency(now.Add(-Keep).UnixMilli())
}

func hashOf(request any) ([]byte, error) {
	b, err := wirejson.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("idempotency: encode the request: %w", err)
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

func decode[T any](b []byte) (out T, replayed bool, err error) {
	if err := json.Unmarshal(b, &out); err != nil {
		return out, false, fmt.Errorf("idempotency: read the kept answer: %w", err)
	}
	return out, true, nil
}
