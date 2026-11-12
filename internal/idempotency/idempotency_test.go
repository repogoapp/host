package idempotency

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/testwait"
)

type answer struct {
	TurnID string `json:"turn_id"`
}

type request struct {
	Prompt string `json:"prompt"`
}

// memory is a Store in a map, which a second Keys over it sees as a
// restarted host does.
type memory struct {
	mu   sync.Mutex
	rows map[id]row
}

type row struct {
	hash, result []byte
	at           int64
}

func newMemory() *memory { return &memory{rows: map[id]row{}} }

func (m *memory) IdempotencyKey(scope, method, key string) ([]byte, []byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[id{scope, method, key}]
	return r.hash, r.result, ok, nil
}

func (m *memory) SaveIdempotencyKey(scope, method, key string, hash, result []byte, at int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[id{scope, method, key}] = row{hash, result, at}
	return nil
}

func (m *memory) DeleteIdempotencyKey(scope, method, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, id{scope, method, key})
	return nil
}

func (m *memory) PruneIdempotency(before int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, r := range m.rows {
		if r.at < before {
			delete(m.rows, k)
		}
	}
	return nil
}

var ctx = context.Background()

func TestARepeatGetsTheFirstAnswerAndSurvivesARestart(t *testing.T) {
	store := newMemory()
	runs := 0
	run := func() (answer, error) { runs++; return answer{TurnID: "t1"}, nil }

	first, replayed, err := Do(ctx, New(store), "device:a", "chats.send", "k1", request{"hi"}, run)
	if err != nil || replayed || first.TurnID != "t1" {
		t.Fatalf("first: %+v %v %v", first, replayed, err)
	}
	again, replayed, err := Do(ctx, New(store), "device:a", "chats.send", "k1", request{"hi"}, run)
	if err != nil || !replayed || again.TurnID != "t1" || runs != 1 {
		t.Fatalf("after a restart: %+v %v %v, %d runs", again, replayed, err, runs)
	}
}

func TestAnotherScopeMethodOrNoKeyRunsAnew(t *testing.T) {
	k := New(newMemory())
	runs := 0
	run := func() (answer, error) { runs++; return answer{}, nil }
	_, _, _ = Do(ctx, k, "device:a", "chats.send", "k1", request{"hi"}, run)
	_, _, _ = Do(ctx, k, "device:b", "chats.send", "k1", request{"hi"}, run)
	_, _, _ = Do(ctx, k, "device:a", "chats.start", "k1", request{"hi"}, run)
	_, _, _ = Do(ctx, k, "device:a", "chats.send", "", request{"hi"}, run)
	_, _, _ = Do(ctx, k, "", "chats.send", "k1", request{"hi"}, run)
	if runs != 5 {
		t.Fatalf("%d runs, want 5", runs)
	}
}

func TestADifferentRequestIsRefused(t *testing.T) {
	k := New(newMemory())
	run := func() (answer, error) { return answer{TurnID: "t1"}, nil }
	_, _, _ = Do(ctx, k, "device:a", "chats.send", "k1", request{"hi"}, run)
	if _, _, err := Do(ctx, k, "device:a", "chats.send", "k1", request{"bye"}, run); !errors.Is(err, ErrReused) {
		t.Fatalf("a different request: %v", err)
	}
}

func TestAFailureIsNotKept(t *testing.T) {
	k := New(newMemory())
	runs := 0
	fail := func() (answer, error) { runs++; return answer{}, errors.New("offline") }
	_, _, _ = Do(ctx, k, "device:a", "chats.send", "k1", request{"hi"}, fail)
	_, _, _ = Do(ctx, k, "device:a", "chats.send", "k1", request{"hi"}, fail)
	if runs != 2 {
		t.Fatalf("%d runs, want 2", runs)
	}
}

func TestAConcurrentRepeatWaitsAndRunsOnce(t *testing.T) {
	k := New(newMemory())
	var runs atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{})
	run := func() (answer, error) {
		runs.Add(1)
		close(started)
		<-release
		return answer{TurnID: "t1"}, nil
	}
	done := make(chan answer)
	go func() {
		a, _, _ := Do(ctx, k, "device:a", "chats.start", "k1", request{"hi"}, run)
		done <- a
	}()
	<-started
	repeat := make(chan answer)
	go func() {
		a, _, _ := Do(ctx, k, "device:a", "chats.start", "k1", request{"hi"}, run)
		repeat <- a
	}()
	close(release)
	if a, b := <-done, <-repeat; a.TurnID != "t1" || b.TurnID != "t1" || runs.Load() != 1 {
		t.Fatalf("first %+v, repeat %+v, %d runs", a, b, runs.Load())
	}
}

func TestAWaiterWhoseContextEndsReturnsWithoutStoppingTheFirst(t *testing.T) {
	k := New(newMemory())
	release := make(chan struct{})
	started := make(chan struct{})
	done := make(chan answer)
	go func() {
		a, _, _ := Do(ctx, k, "device:a", "chats.start", "k1", request{"hi"}, func() (answer, error) {
			close(started)
			<-release
			return answer{TurnID: "t1"}, nil
		})
		done <- a
	}()
	<-started
	waiting, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, _, err := Do(waiting, k, "device:a", "chats.start", "k1", request{"hi"}, func() (answer, error) {
		t.Error("the repeat ran")
		return answer{}, nil
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the waiter: %v", err)
	}
	close(release)
	if a := <-done; a.TurnID != "t1" {
		t.Fatalf("the first: %+v", a)
	}
}

func TestAPanicReleasesTheWaiters(t *testing.T) {
	k := New(newMemory())
	started := make(chan struct{})
	release := make(chan struct{})
	go func() {
		defer func() { _ = recover() }()
		_, _, _ = Do(ctx, k, "device:a", "chats.start", "k1", request{"hi"}, func() (answer, error) {
			close(started)
			<-release
			panic("boom")
		})
	}()
	<-started
	result := make(chan error)
	go func() {
		_, _, err := Do(ctx, k, "device:a", "chats.start", "k1", request{"hi"}, func() (answer, error) { return answer{}, nil })
		result <- err
	}()
	testwait.For(t, "the repeat to wait", func() bool {
		k.mu.Lock()
		defer k.mu.Unlock()
		c := k.running[id{"device:a", "chats.start", "k1"}]
		return c != nil && c.waiters == 1
	})
	close(release)
	select {
	case err := <-result:
		if !errors.Is(err, errUnfinished) {
			t.Fatalf("the waiter: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the waiter is stuck after a panic")
	}
}

func TestOldAnswersArePruned(t *testing.T) {
	store := newMemory()
	now := time.Unix(1_000_000, 0)
	k := New(store)
	k.now = func() time.Time { return now }
	k.pruned = time.Time{}
	_, _, _ = Do(ctx, k, "device:a", "chats.send", "old", request{"hi"}, func() (answer, error) { return answer{}, nil })
	now = now.Add(Keep + time.Hour)
	_, _, _ = Do(ctx, k, "device:a", "chats.send", "new", request{"hi"}, func() (answer, error) { return answer{}, nil })
	if _, _, ok, _ := store.IdempotencyKey("device:a", "chats.send", "old"); ok {
		t.Fatal("a day-old answer survived")
	}
	if _, _, ok, _ := store.IdempotencyKey("device:a", "chats.send", "new"); !ok {
		t.Fatal("a new answer was pruned")
	}
}

func TestForgetLetsTheKeyRunAnew(t *testing.T) {
	k := New(newMemory())
	runs := 0
	run := func() (answer, error) { runs++; return answer{}, nil }
	_, _, _ = Do(ctx, k, "device:a", "chats.start", "k1", request{"hi"}, run)
	if err := k.Forget("device:a", "chats.start", "k1"); err != nil {
		t.Fatal(err)
	}
	_, _, _ = Do(ctx, k, "device:a", "chats.start", "k1", request{"hi"}, run)
	if runs != 2 {
		t.Fatalf("%d runs, want 2", runs)
	}
}
