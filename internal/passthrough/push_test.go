package passthrough

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/pushgate"
)

func anyApp([]byte, []byte, []byte, []byte, string, time.Time) error { return nil }

// grantedPush is a push to token with a fresh phone's grant for host.
func grantedPush(t *testing.T, host device.ID, token string) pushgate.PushRequest {
	t.Helper()
	phone, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	target := phone.GrantPush(host, device.PushTarget{Service: device.PushAPNs, Token: token, Environment: pushgate.EnvironmentSandbox, GrantedAt: time.Now().UnixMilli()})
	return pushgate.PushRequest{Token: token, Environment: pushgate.EnvironmentSandbox, Payload: []byte(`{"aps":{}}`),
		Grant: pushgate.PushGrant{Device: phone.ID, PublicKey: phone.Public, Signature: target.Grant, GrantedAt: target.GrantedAt}}
}

func withGate(deliver pushgate.Deliver) options {
	return options{relay: func(r *Relay) { r.Push = pushgate.New(deliver, anyApp) }}
}

func rpcCode(err error) int {
	var e *jsonrpc.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

func TestAPushOnTheLinkReachesAPNs(t *testing.T) {
	sent := make(chan pushgate.PushRequest, 1)
	s := startWith(t, withGate(func(_ context.Context, req pushgate.PushRequest) error { sent <- req; return nil }))
	if err := s.listener.Push(t.Context(), grantedPush(t, s.host.ID, "abcd")); err != nil {
		t.Fatal(err)
	}
	if got := <-sent; got.Token != "abcd" {
		t.Fatalf("APNs got %+v", got)
	}
}

// The relay checks the grant against the host its link proved, so a host
// can't push with a grant another host was given.
func TestAPushNeedsAGrantForTheLinkedHost(t *testing.T) {
	var sent atomic.Int32
	s := startWith(t, withGate(func(context.Context, pushgate.PushRequest) error { sent.Add(1); return nil }))
	other, _ := device.Generate()
	if err := s.listener.Push(t.Context(), grantedPush(t, other.ID, "abcd")); rpcCode(err) != jsonrpc.CodeDenied {
		t.Fatalf("another host's grant: %v, want denied", err)
	}
	if sent.Load() != 0 {
		t.Fatal("APNs was called with another host's grant")
	}
}

func TestADeadTokenComesBackAsNotFound(t *testing.T) {
	s := startWith(t, withGate(func(context.Context, pushgate.PushRequest) error { return pushgate.ErrUnregistered }))
	if err := s.listener.Push(t.Context(), grantedPush(t, s.host.ID, "abcd")); rpcCode(err) != jsonrpc.CodeNotFound {
		t.Fatalf("dead token: %v, want not found", err)
	}
}

func TestARelayWithoutAPNsRefusesPushes(t *testing.T) {
	s := start(t)
	if err := s.listener.Push(t.Context(), grantedPush(t, s.host.ID, "abcd")); rpcCode(err) != jsonrpc.CodeUnavailable {
		t.Fatalf("keyless relay: %v, want unavailable", err)
	}
	// Refusing a push leaves the link up for phones.
	if !s.relay.Linked(s.host.ID) {
		t.Fatal("the link dropped after a refused push")
	}
}

func TestPushesAreRateLimitedPerHost(t *testing.T) {
	var sent atomic.Int32
	s := startWith(t, withGate(func(context.Context, pushgate.PushRequest) error { sent.Add(1); return nil }))
	for i := range pushgate.HostBurst {
		// Distinct tokens, so the per-token budget never applies.
		if err := s.listener.Push(t.Context(), grantedPush(t, s.host.ID, string(rune('a'+i%26))+string(rune('a'+i/26)))); err != nil {
			t.Fatalf("push %d refused: %v", i, err)
		}
	}
	if err := s.listener.Push(t.Context(), grantedPush(t, s.host.ID, "zzzz")); rpcCode(err) != jsonrpc.CodeDenied {
		t.Fatalf("push past the burst: %v, want denied", err)
	}
	if n := sent.Load(); n != pushgate.HostBurst {
		t.Fatalf("APNs saw %d pushes, want %d", n, pushgate.HostBurst)
	}
}

// A slow APNs call holds up neither phones reaching the host nor forever.
func TestASlowPushDoesNotStallPhones(t *testing.T) {
	release := make(chan struct{})
	bounded := make(chan bool, 1)
	s := startWith(t, withGate(func(ctx context.Context, _ pushgate.PushRequest) error {
		_, ok := ctx.Deadline()
		bounded <- ok
		<-release
		return nil
	}))
	done := make(chan error, 1)
	go func() { done <- s.listener.Push(t.Context(), grantedPush(t, s.host.ID, "abcd")) }()
	if !<-bounded {
		t.Error("APNs was called with no deadline")
	}
	resp, err := s.phone(string(s.host.ID)+suffix, s.pin)
	if err != nil {
		t.Fatalf("a phone couldn't connect while a push was in flight: %v", err)
	}
	resp.Body.Close()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestAPushWithNoLinkFailsAtOnce(t *testing.T) {
	l := &Listener{pending: map[uint64]chan relayLine{}}
	if err := l.Push(t.Context(), pushgate.PushRequest{}); rpcCode(err) != jsonrpc.CodeUnavailable {
		t.Fatalf("no link: %v, want unavailable", err)
	}
}
