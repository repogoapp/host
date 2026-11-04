package host_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/host"
	"github.com/repogoapp/host/internal/testhost"
)

// A worker stuck past the grace lets Close go on, so an update still restarts.
func TestWaitWithinGivesUpOnAStuckWorker(t *testing.T) {
	var wg sync.WaitGroup
	release := make(chan struct{})
	wg.Go(func() { <-release })

	if host.WaitWithin(&wg, 10*time.Millisecond) {
		t.Fatal("waited out a worker that never stopped")
	}
	close(release)
	if !host.WaitWithin(&wg, time.Minute) {
		t.Fatal("gave up on a worker that stopped")
	}
}

// runUntilStopped opens session s1 and runs until its turn is cancelled.
type runUntilStopped struct{ opened chan struct{} }

func (runUntilStopped) Kind() agent.Kind { return agent.KindClaude }
func (runUntilStopped) Available() error { return nil }
func (a runUntilStopped) Send(ctx context.Context, _ agent.TurnRequest, io agent.TurnIO) (agent.Result, error) {
	io.Session("s1", "")
	close(a.opened)
	<-ctx.Done()
	return agent.Result{}, ctx.Err()
}

// Close stops a running turn and lets it settle while the store it settles
// into is still open, rather than leaving it running under a closed database.
func TestCloseStopsRunningTurns(t *testing.T) {
	a := runUntilStopped{opened: make(chan struct{})}
	th := testhost.New(t, withAdapter(a))
	st, err := th.Services.Runner.Send(agent.TurnRequest{
		ChatID: "claude:s1", SessionID: "s1", Cwd: th.Root, Agent: agent.KindClaude, Prompt: "go",
	})
	if err != nil {
		t.Fatal(err)
	}
	<-a.opened

	th.Close()

	got, err := th.Services.Runner.Status(st.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != agent.StateStopped {
		t.Fatalf("turn %s after Close, want %s", got.State, agent.StateStopped)
	}
}
