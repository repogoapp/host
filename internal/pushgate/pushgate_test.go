package pushgate

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/appattest"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/jsonrpc"
)

var host = device.ID("00000000000000000000000000000001")

func anyApp([]byte, []byte, []byte, []byte, string, time.Time) error { return nil }

// request is a push to token carrying a fresh phone's grant for to.
func request(t *testing.T, to device.ID, token string, grantedAt time.Time) PushRequest {
	t.Helper()
	phone, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return signed(phone, to, token, grantedAt)
}

func signed(phone *device.Identity, to device.ID, token string, grantedAt time.Time) PushRequest {
	target := phone.GrantPush(to, device.PushTarget{Service: device.PushAPNs, Token: token, Environment: EnvironmentSandbox, GrantedAt: grantedAt.UnixMilli()})
	return PushRequest{Token: token, Environment: EnvironmentSandbox, Payload: []byte(`{}`),
		Grant: PushGrant{Device: phone.ID, PublicKey: phone.Public, Signature: target.Grant, GrantedAt: target.GrantedAt}}
}

func code(err error) int {
	var e *jsonrpc.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

func TestSendReachesAPNs(t *testing.T) {
	var got PushRequest
	g := New(func(_ context.Context, req PushRequest) error { got = req; return nil }, anyApp)
	req := request(t, host, "abcd", time.Now())
	req.CollapseID, req.Payload = "c1", []byte(`{"aps":{"alert":"hi"}}`)
	if err := g.Send(t.Context(), host, req); err != nil {
		t.Fatal(err)
	}
	if got.Token != "abcd" || got.CollapseID != "c1" || !strings.Contains(string(got.Payload), `"alert":"hi"`) {
		t.Fatalf("APNs got %+v", got)
	}
}

func TestSelfSignedIdentityCannotClaimAPNsToken(t *testing.T) {
	v, err := appattest.New("ABCDEFGHIJ.app.repogo")
	if err != nil {
		t.Fatal(err)
	}
	var sent atomic.Int32
	g := New(func(context.Context, PushRequest) error { sent.Add(1); return nil }, v.Verify)
	if err := g.Send(t.Context(), host, request(t, host, "abcd", time.Now())); code(err) != jsonrpc.CodeDenied {
		t.Fatalf("self-signed grant: %v", err)
	}
	if sent.Load() != 0 {
		t.Fatal("unattested token reached APNs")
	}
}

// Apple's certificates expire long before a grant does, so they are checked
// as of the grant's signed time.
func TestSendChecksAppleEvidenceAtTheGrantTime(t *testing.T) {
	var at time.Time
	g := New(func(context.Context, PushRequest) error { return nil },
		func(_, _, _, _ []byte, _ string, signedAt time.Time) error { at = signedAt; return nil })
	granted := time.Now().Add(-3 * 24 * time.Hour).Truncate(time.Millisecond)
	if err := g.Send(t.Context(), host, request(t, host, "abcd", granted)); err != nil {
		t.Fatal(err)
	}
	if !at.Equal(granted) {
		t.Fatalf("verified at %v, want the grant time %v", at, granted)
	}
}

func TestSendWithoutAnAppVerifierIsDenied(t *testing.T) {
	g := New(func(context.Context, PushRequest) error { return nil }, nil)
	if err := g.Send(t.Context(), host, request(t, host, "abcd", time.Now())); code(err) != jsonrpc.CodeDenied {
		t.Fatalf("no verifier: %v, want denied", err)
	}
}

func TestSendReportsDeadTokens(t *testing.T) {
	g := New(func(context.Context, PushRequest) error { return ErrUnregistered }, anyApp)
	if err := g.Send(t.Context(), host, request(t, host, "abcd", time.Now())); code(err) != jsonrpc.CodeNotFound {
		t.Fatalf("dead token = %v, want not found", err)
	}
}

func TestSendWithoutKeyIsUnavailable(t *testing.T) {
	g := New(nil, anyApp)
	if err := g.Send(t.Context(), host, request(t, host, "abcd", time.Now())); code(err) != jsonrpc.CodeUnavailable {
		t.Fatalf("keyless gate = %v, want unavailable", err)
	}
}

// An environment APNs does not have is the caller's mistake, not the relay's.
func TestSendRefusesAnUnknownEnvironment(t *testing.T) {
	var sent atomic.Int32
	g := New(func(context.Context, PushRequest) error { sent.Add(1); return nil }, anyApp)
	req := request(t, host, "abcd", time.Now())
	req.Environment = "staging"
	if err := g.Send(t.Context(), host, req); code(err) != jsonrpc.CodeInvalidParams {
		t.Fatalf("unknown environment = %v, want invalid params", err)
	}
	if sent.Load() != 0 {
		t.Fatal("APNs was called for an unknown environment")
	}
}

// A push goes only with the token's phone's leave for this host: a grant
// signed by another phone, naming another host or token, or dated outside
// its window is refused, and so is none at all.
func TestSendNeedsThePhonesGrant(t *testing.T) {
	var sent atomic.Int32
	g := New(func(context.Context, PushRequest) error { sent.Add(1); return nil }, anyApp)
	phone, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	other := device.ID("00000000000000000000000000000002")
	anotherToken := signed(phone, host, "ffff", time.Now())
	anotherToken.Token = "abcd"
	bad := map[string]PushRequest{
		"no grant":        {Token: "abcd", Environment: EnvironmentSandbox, Payload: []byte(`{}`)},
		"another host":    signed(phone, other, "abcd", time.Now()),
		"another token":   anotherToken,
		"stale":           signed(phone, host, "abcd", time.Now().Add(-grantMaxAge-time.Hour)),
		"from the future": signed(phone, host, "abcd", time.Now().Add(grantSkew+time.Hour)),
	}
	for name, req := range bad {
		if err := g.Send(t.Context(), host, req); code(err) != jsonrpc.CodeDenied {
			t.Errorf("%s: %v, want denied", name, err)
		}
	}
	if sent.Load() != 0 {
		t.Fatal("APNs was called without a good grant")
	}
	if err := g.Send(t.Context(), host, signed(phone, host, "abcd", time.Now())); err != nil {
		t.Fatalf("a good grant was refused: %v", err)
	}
}

// One token takes tokenBurst pushes a minute from every host together.
func TestSendIsRateLimitedPerToken(t *testing.T) {
	g := New(func(context.Context, PushRequest) error { return nil }, anyApp)
	other := device.ID("00000000000000000000000000000002")
	for i := range tokenBurst {
		from := host
		if i%2 == 1 {
			from = other
		}
		if err := g.Send(t.Context(), from, request(t, from, "abcd", time.Now())); err != nil {
			t.Fatalf("push %d refused: %v", i, err)
		}
	}
	if err := g.Send(t.Context(), host, request(t, host, "abcd", time.Now())); code(err) != jsonrpc.CodeDenied {
		t.Fatalf("push past the token budget: %v, want denied", err)
	}
	if err := g.Send(t.Context(), host, request(t, host, "ef01", time.Now())); err != nil {
		t.Fatalf("another token was refused: %v", err)
	}
}

func TestWindowAllowsLimitPerMinute(t *testing.T) {
	var w Window
	for i := range HostBurst {
		if !w.Allow(HostBurst) {
			t.Fatalf("event %d refused", i)
		}
	}
	if w.Allow(HostBurst) {
		t.Fatalf("event %d allowed past the burst", HostBurst+1)
	}
}
