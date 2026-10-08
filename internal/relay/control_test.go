package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/handshake"
	"github.com/repogo/host/internal/jsonrpc"
)

func pushHarness(t *testing.T, push func(context.Context, PushRequest) error) *harness {
	t.Helper()
	return newHarnessWith(t, Config{Push: push})
}

// control sends one push.send and returns the relay's reply.
func (p *peer) control(params string) *jsonrpc.Message {
	p.t.Helper()
	p.send(ControlID, `{"jsonrpc":"2.0","id":"1","method":"push.send","params":`+params+`}`)
	from, body, err := p.recv(2 * time.Second)
	if err != nil {
		p.t.Fatalf("no control reply: %v", err)
	}
	if from != ControlID {
		p.t.Fatalf("reply came from %s, want the relay", from)
	}
	msg, err := jsonrpc.Decode([]byte(body))
	if err != nil {
		p.t.Fatal(err)
	}
	return msg
}

func TestPushSendReachesAPNsAndReplies(t *testing.T) {
	sent := make(chan PushRequest, 1)
	h := pushHarness(t, func(_ context.Context, req PushRequest) error { sent <- req; return nil })
	host, _ := h.connect("g", handshake.RoleRuntime)

	reply := host.control(strings.Replace(pushParams(t, host, "abcd", "sandbox", `"collapse_id":"c1",`), `"payload":{}`, `"payload":{"aps":{"alert":"hi"}}`, 1))
	if reply.Error != nil {
		t.Fatalf("push.send failed: %v", reply.Error)
	}
	if got := <-sent; got.Token != "abcd" || got.Environment != "sandbox" || got.CollapseID != "c1" ||
		!strings.Contains(string(got.Payload), `"alert":"hi"`) {
		t.Fatalf("relay handed APNs %+v", got)
	}
}

func TestPushSendReportsDeadTokens(t *testing.T) {
	h := pushHarness(t, func(context.Context, PushRequest) error { return ErrUnregistered })
	host, _ := h.connect("g", handshake.RoleRuntime)
	reply := host.control(pushParams(t, host, "abcd", "sandbox", ""))
	if reply.Error == nil || reply.Error.Code != jsonrpc.CodeNotFound {
		t.Fatalf("dead token reply = %v, want not found", reply.Error)
	}
}

func TestPushSendWithoutKeyIsUnavailable(t *testing.T) {
	h := newHarness(t)
	host, _ := h.connect("g", handshake.RoleRuntime)
	reply := host.control(pushParams(t, host, "abcd", "sandbox", ""))
	if reply.Error == nil || reply.Error.Code != jsonrpc.CodeUnavailable {
		t.Fatalf("keyless relay reply = %v, want unavailable", reply.Error)
	}
}

func TestPushSendIsRateLimited(t *testing.T) {
	var sent atomic.Int32
	h := pushHarness(t, func(context.Context, PushRequest) error { sent.Add(1); return nil })
	host, _ := h.connect("g", handshake.RoleRuntime)
	for i := 0; i < pushBurst; i++ {
		if reply := host.control(pushParams(t, host, fmt.Sprintf("%04x", i), "sandbox", "")); reply.Error != nil {
			t.Fatalf("push %d refused: %v", i, reply.Error)
		}
	}
	if reply := host.control(pushParams(t, host, "ffff", "sandbox", "")); reply.Error == nil || reply.Error.Code != jsonrpc.CodeDenied {
		t.Fatalf("push past the burst = %v, want denied", reply.Error)
	}
	if n := sent.Load(); n != pushBurst {
		t.Fatalf("APNs saw %d pushes, want %d", n, pushBurst)
	}
}

// A control frame never reaches another device, whatever it claims.
func TestControlFramesAreNotRouted(t *testing.T) {
	h := pushHarness(t, func(context.Context, PushRequest) error { return nil })
	host, _ := h.connect("g", handshake.RoleRuntime)
	phone, _ := h.connect("g", handshake.RoleClient)
	host.control(pushParams(t, host, "abcd", "sandbox", ""))
	if _, body, err := phone.recv(200 * time.Millisecond); err == nil {
		t.Fatalf("phone received a control frame: %s", body)
	}
}

// An environment APNs does not have is the caller's mistake, not the relay's.
func TestPushSendRefusesAnUnknownEnvironment(t *testing.T) {
	var sent atomic.Int32
	h := pushHarness(t, func(context.Context, PushRequest) error { sent.Add(1); return nil })
	host, _ := h.connect("g", handshake.RoleRuntime)
	reply := host.control(`{"token":"t","environment":"staging","payload":{}}`)
	if reply.Error == nil || reply.Error.Code != jsonrpc.CodeInvalidParams {
		t.Fatalf("unknown environment = %v, want invalid params", reply.Error)
	}
	if sent.Load() != 0 {
		t.Fatal("APNs was called for an unknown environment")
	}
}

// A slow APNs call holds up neither the host's routed frames nor forever.
func TestASlowPushDoesNotStallRoutedFrames(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	free := func() { once.Do(func() { close(release) }) }
	t.Cleanup(free)
	bounded := make(chan bool, 1)
	h := pushHarness(t, func(ctx context.Context, _ PushRequest) error {
		_, ok := ctx.Deadline()
		bounded <- ok
		<-release
		return nil
	})
	host, _ := h.connect("g", handshake.RoleRuntime)
	phone, _ := h.connect("g", handshake.RoleClient)

	host.send(ControlID, `{"jsonrpc":"2.0","id":"1","method":"push.send","params":`+pushParams(t, host, "abcd", "sandbox", "")+`}`)
	if !<-bounded {
		t.Error("APNs was called with no deadline")
	}
	host.send(phone.id, "meanwhile")
	if _, body, err := phone.recv(3 * time.Second); err != nil || body != "meanwhile" {
		t.Fatalf("phone got %q %v while a push was in flight", body, err)
	}
	free()
	if from, _, err := host.recv(3 * time.Second); err != nil || from != ControlID {
		t.Fatalf("no push reply: %s %v", from, err)
	}
}

// pushParams is push.send's params for token, carrying a fresh phone's grant
// for host; extra is further JSON fields, each with a trailing comma.
func pushParams(t *testing.T, host *peer, token, environment, extra string) string {
	t.Helper()
	phone, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return pushParamsSigned(t, phone, host.id, token, environment, time.Now().UnixMilli(), extra)
}

func pushParamsSigned(t *testing.T, phone *device.Identity, host device.ID, token, environment string, grantedAt int64, extra string) string {
	t.Helper()
	target := phone.GrantPush(host, device.PushTarget{Token: token, Environment: environment, GrantedAt: grantedAt})
	grant, err := json.Marshal(PushGrant{Device: phone.ID, PublicKey: phone.Public, Signature: target.Grant, GrantedAt: target.GrantedAt})
	if err != nil {
		t.Fatal(err)
	}
	return `{"token":"` + token + `","environment":"` + environment + `",` + extra + `"payload":{},"grant":` + string(grant) + `}`
}

// A push goes only with the token's phone's leave for this host: a grant
// signed by another phone, naming another host or token, or dated outside
// its window is refused, and so is none at all.
func TestPushSendNeedsThePhonesGrant(t *testing.T) {
	var sent atomic.Int32
	h := pushHarness(t, func(context.Context, PushRequest) error { sent.Add(1); return nil })
	host, _ := h.connect("g", handshake.RoleRuntime)
	other, _ := h.connect("g", handshake.RoleRuntime)
	phone, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	good := pushParamsSigned(t, phone, host.id, "abcd", "sandbox", now, "")
	bad := map[string]string{
		"no grant":        `{"token":"abcd","environment":"sandbox","payload":{}}`,
		"another host":    pushParamsSigned(t, phone, other.id, "abcd", "sandbox", now, ""),
		"another token":   strings.Replace(pushParamsSigned(t, phone, host.id, "ffff", "sandbox", now, ""), `"token":"ffff"`, `"token":"abcd"`, 1),
		"stale":           pushParamsSigned(t, phone, host.id, "abcd", "sandbox", time.Now().Add(-grantMaxAge-time.Hour).UnixMilli(), ""),
		"from the future": pushParamsSigned(t, phone, host.id, "abcd", "sandbox", time.Now().Add(grantSkew+time.Hour).UnixMilli(), ""),
	}
	for name, params := range bad {
		if reply := host.control(params); reply.Error == nil || reply.Error.Code != jsonrpc.CodeDenied {
			t.Errorf("%s: reply %v, want denied", name, reply.Error)
		}
	}
	if sent.Load() != 0 {
		t.Fatal("APNs was called without a good grant")
	}
	if reply := host.control(good); reply.Error != nil {
		t.Fatalf("a good grant was refused: %v", reply.Error)
	}
}

// One token takes tokenBurst pushes a minute from everyone together.
func TestPushSendIsRateLimitedPerToken(t *testing.T) {
	h := pushHarness(t, func(context.Context, PushRequest) error { return nil })
	a, _ := h.connect("g", handshake.RoleRuntime)
	b, _ := h.connect("g", handshake.RoleRuntime)
	for i := 0; i < tokenBurst; i++ {
		from := a
		if i%2 == 1 {
			from = b
		}
		if reply := from.control(pushParams(t, from, "abcd", "sandbox", "")); reply.Error != nil {
			t.Fatalf("push %d refused: %v", i, reply.Error)
		}
	}
	if reply := a.control(pushParams(t, a, "abcd", "sandbox", "")); reply.Error == nil || reply.Error.Code != jsonrpc.CodeDenied {
		t.Fatalf("push past the token budget: %v, want denied", reply.Error)
	}
	if reply := a.control(pushParams(t, a, "ef01", "sandbox", "")); reply.Error != nil {
		t.Fatalf("another token was refused: %v", reply.Error)
	}
}
