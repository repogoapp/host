package remotehost

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/testwait"
)

// sender answers calls by script, and counts them.
type sender struct {
	mu      sync.Mutex
	targets []string
	stdin   []string
	answers []answer
	// block, when set, holds every call until it's closed.
	block  chan struct{}
	inside int
	most   int
}

func (s *sender) send(ctx context.Context, alias, command string, stdin []byte) ([]byte, []byte, int) {
	s.mu.Lock()
	s.targets = append(s.targets, alias)
	s.stdin = append(s.stdin, string(stdin))
	s.inside++
	s.most = max(s.most, s.inside)
	var a answer
	if len(s.answers) > 0 {
		a, s.answers = s.answers[0], s.answers[1:]
	}
	s.mu.Unlock()
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
		}
	}
	s.mu.Lock()
	s.inside--
	s.mu.Unlock()
	// As ssh does when its call is ended: no answer, and no exit code of its own.
	if ctx.Err() != nil {
		return nil, nil, -1
	}
	if command != callCommand {
		return nil, []byte("unexpected command"), 1
	}
	return []byte(a.stdout), []byte(a.stderr), a.code
}

// listedStudio is a service whose user asked for the list, which holds
// studio, answering as H1, and work, a server with no RepoGo; neither is
// switched on. The machine it runs on comes with it.
func listedStudio(t *testing.T, s *sender) (*Service, *fake) {
	t.Helper()
	f := &fake{
		config:  "Host studio\n  HostName 100.73.205.82\nHost work\n",
		answers: map[string]answer{"studio": {stdout: `{"host_id": "H1", "version": "0.1.50"}`}},
	}
	d := f.deps("")
	d.Send = s.send
	svc := NewService(d)
	if _, err := svc.SetListing(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	return svc, f
}

// onStudio is listedStudio with studio switched on.
func onStudio(t *testing.T, s *sender) (*Service, *fake) {
	t.Helper()
	svc, f := listedStudio(t, s)
	if _, err := svc.Authorize(context.Background(), "studio", true); err != nil {
		t.Fatal(err)
	}
	return svc, f
}

func TestCallDialsTheAliasAndAnswersTheResult(t *testing.T) {
	s := &sender{answers: []answer{{stdout: `{"result": {"agents": []}}`}}}
	svc, _ := onStudio(t, s)
	got, err := svc.Call(context.Background(), "studio", Request{Method: "agents.list", From: "a chat"}, true)
	if err != nil || string(got) != `{"agents": []}` {
		t.Fatalf("Call = %s, %v", got, err)
	}
	// Addressed to the host that answered when it was checked, so another
	// that the connection reaches by then refuses it.
	if s.targets[0] != "studio" || s.stdin[0] != `{"method":"agents.list","from":"a chat","to":"H1"}` {
		t.Fatalf("dialed %v with %v, want the alias and the call on stdin, addressed to H1", s.targets, s.stdin)
	}
	// By its id in any case; never by the host_id it claims for itself.
	s.answers = []answer{{stdout: `{"result": 1}`}}
	if _, err := svc.Call(context.Background(), "STUDIO", Request{}, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Call(context.Background(), "H1", Request{}, true); !errors.Is(err, errkind.ErrDenied) {
		t.Errorf("by host id: %v, want denied", err)
	}
}

// A host that is off and a name that isn't listed are refused in the same
// words, without a call: asking tells a caller nothing about the user's SSH
// connections.
func TestOnlyAHostSwitchedOnTakesACall(t *testing.T) {
	s := &sender{}
	svc, f := listedStudio(t, s)
	said := map[string]string{}
	for _, id := range []string{"work", "nowhere"} {
		_, err := svc.Call(context.Background(), id, Request{}, true)
		if !errors.Is(err, errkind.ErrDenied) {
			t.Fatalf("%s: %v, want denied", id, err)
		}
		said[id] = strings.ReplaceAll(err.Error(), id, "X")
	}
	if said["work"] != said["nowhere"] || len(s.targets)+len(f.ran) != 0 {
		t.Fatalf("refusals %q, dialed %v and %v; want the same words and nothing dialed", said, s.targets, f.ran)
	}
	// Nor does a guess make this host look again: asking costs nothing and shows nothing.
	if f.read != 1 {
		t.Fatalf("read the SSH file %d times, want only the once the user asked for the list", f.read)
	}
}

func TestAReadIsRetriedOnceAndAWriteNever(t *testing.T) {
	s := &sender{answers: []answer{{code: 255, stderr: "mux_client_request_session: read from master failed"}, {stdout: `{"result": "ok"}`}}}
	svc, _ := onStudio(t, s)
	if got, err := svc.Call(context.Background(), "studio", Request{}, true); err != nil || string(got) != `"ok"` || len(s.targets) != 2 {
		t.Fatalf("read: %s, %v after %d calls; want ok after a retry", got, err, len(s.targets))
	}
	s.targets = nil
	s.answers = []answer{{code: 255, stderr: "Connection closed"}, {stdout: `{"result": "ran twice"}`}}
	_, err := svc.Call(context.Background(), "studio", Request{}, false)
	if err == nil || !strings.Contains(err.Error(), "unknown") || len(s.targets) != 1 {
		t.Fatalf("write: %v after %d calls; want outcome unknown after one", err, len(s.targets))
	}
}

func TestCallNamesTheHostInItsErrors(t *testing.T) {
	s := &sender{answers: []answer{
		{code: 1, stderr: "repogo: host: chats.start from another host isn't supported yet\n"},
		{code: 127, stderr: "sh: repogo: not found"},
		{code: 1, stderr: `repogo: unknown command "call"; run repogo help`},
		{code: ExitNotRunning, stderr: "repogo: whatever it says"},
	}}
	svc, _ := onStudio(t, s)
	for _, want := range []string{"studio: chats.start from another host isn't supported yet",
		"RepoGo isn't installed on studio", "too old", "isn't running on studio right now"} {
		if _, err := svc.Call(context.Background(), "studio", Request{}, false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err %v, want %q", err, want)
		}
	}
}

// A host that is on but can't be used is refused with its reason, no call sent.
func TestAHostThatIsntReadyIsRefusedWithWhy(t *testing.T) {
	s := &sender{}
	svc, _ := listedStudio(t, s)
	if _, err := svc.Authorize(context.Background(), "work", true); err != nil {
		t.Fatal(err)
	}
	_, err := svc.Call(context.Background(), "work", Request{}, true)
	if !errors.Is(err, errkind.ErrUnavailable) || !strings.Contains(err.Error(), "Could not resolve") || len(s.targets) != 0 {
		t.Fatalf("Call = %v after %d calls, want its reason and none", err, len(s.targets))
	}
}

func TestAtMostEightCallsGoToOneHostAtOnce(t *testing.T) {
	s := &sender{block: make(chan struct{})}
	for range 10 {
		s.answers = append(s.answers, answer{stdout: `{"result": null}`})
	}
	svc, _ := onStudio(t, s)
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			svc.Call(context.Background(), "studio", Request{}, true)
		}()
	}
	testwait.For(t, "eight calls in flight", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.inside >= perHost
	})
	close(s.block)
	wg.Wait()
	if s.most != perHost {
		t.Fatalf("at most %d at once, want %d", s.most, perHost)
	}
}

// A host that was offline at the last look is looked at again once that look
// is old, so one that came back takes the call.
func TestACallLooksAgainAtAHostThatWasntReady(t *testing.T) {
	s := &sender{answers: []answer{{stdout: `{"result": 1}`}}}
	svc, f := listedStudio(t, s)
	now := time.UnixMilli(1_000_000)
	svc.now = func() time.Time { return now }
	up := f.answers["studio"]
	f.answers["studio"] = answer{stderr: "ssh: connect: Operation timed out", code: 255}
	if _, err := svc.Authorize(context.Background(), "studio", true); err != nil {
		t.Fatal(err)
	}
	f.answers["studio"] = up
	if _, err := svc.Call(context.Background(), "studio", Request{}, true); !errors.Is(err, errkind.ErrUnavailable) {
		t.Fatalf("Call under Reuse = %v, want the recent look's offline", err)
	}
	now = now.Add(Reuse)
	if _, err := svc.Call(context.Background(), "studio", Request{}, true); err != nil || len(s.targets) != 1 {
		t.Fatalf("Call past Reuse = %v after %d calls, want it sent", err, len(s.targets))
	}
}

// Switching a host off ends the login kept open to it and every call to it:
// the ones running, and one still waiting its turn, which is never sent.
func TestSwitchingOffStopsWaitingCallsAndHangsUp(t *testing.T) {
	s := &sender{block: make(chan struct{})}
	svc, f := onStudio(t, s)
	errs := make(chan error, perHost+1)
	for range perHost + 1 {
		go func() {
			_, err := svc.Call(context.Background(), "studio", Request{}, true)
			errs <- err
		}()
	}
	testwait.For(t, "the host's calls in flight", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.inside >= perHost
	})
	if _, err := svc.Authorize(context.Background(), "studio", false); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f.closed, "studio") {
		t.Fatalf("closed %v, want the login to studio ended", f.closed)
	}
	// Off ends the calls that were running too, without waiting on them.
	for range perHost + 1 {
		if err := <-errs; !errors.Is(err, errkind.ErrDenied) || !strings.Contains(err.Error(), "was switched off") {
			t.Fatalf("a call cut off by the switch: %v, want it refused as switched off", err)
		}
	}
	if len(s.targets) != perHost {
		t.Fatalf("%d calls sent, want only the %d that had started", len(s.targets), perHost)
	}
	close(s.block)
}
