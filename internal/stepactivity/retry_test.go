package stepactivity

import (
	"context"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/jsonrpc"
)

// A phone the relay keeps refusing is retried less and less, and an ended
// card is given up on once the phones have dismissed it, so one bad phone
// can't spend the host's push budget.
func TestRefusedPushesBackOff(t *testing.T) {
	f := setup(t)
	f.set("vercel", deploy)
	f.register(device.PushSteps, "vercel/deploy:web", "77aa")
	done := deploy
	done.Status = Done
	f.sender.Reply = &jsonrpc.Error{Code: jsonrpc.CodeDenied, Message: "the grant carries no Play Integrity token"}
	if err := f.d.Set("vercel", done); err != nil {
		t.Fatal(err)
	}
	start := f.now
	sent := func() int { return len(f.sender.Calls) }
	var tries []time.Duration
	for elapsed := time.Duration(0); elapsed <= 20*time.Minute; elapsed += time.Second {
		f.now = start.Add(elapsed)
		before := sent()
		f.d.flush(context.Background())
		if sent() > before {
			tries = append(tries, elapsed)
		}
	}
	// 1 s for the end, then waits of 5, 10, 20… s up to 5 min, then none past 15 min.
	if len(tries) < 4 || len(tries) > 12 {
		t.Fatalf("%d tries at %v", len(tries), tries)
	}
	for i := 2; i < len(tries); i++ {
		if gap, prev := tries[i]-tries[i-1], tries[i-1]-tries[i-2]; gap < prev {
			t.Fatalf("waits shrank: %v", tries)
		}
	}
	if last := tries[len(tries)-1]; last > endedFor+maxRetryGap {
		t.Fatalf("still trying at %v", last)
	}
	f.d.mu.Lock()
	defer f.d.mu.Unlock()
	if len(f.d.live) != 0 {
		t.Fatal("the ended card was kept")
	}
}

// A new state from the plugin goes at once, whatever the backoff.
func TestNewStateSkipsTheBackoff(t *testing.T) {
	f := setup(t)
	f.sender.Reply = &jsonrpc.Error{Code: jsonrpc.CodeDenied, Message: "refused"}
	f.set("vercel", deploy)
	for range 4 {
		f.now = f.now.Add(updateInterval)
		f.d.flush(context.Background())
	}
	before := len(f.sender.Calls)
	f.now = f.now.Add(updateInterval)
	if err := f.d.Set("vercel", deployAt(2, "Deploying", "")); err != nil {
		t.Fatal(err)
	}
	f.d.flush(context.Background())
	if len(f.sender.Calls) == before {
		t.Fatal("a new state waited out the backoff")
	}
}
