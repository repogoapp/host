package relay

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

	reply := host.control(`{"token":"abcd","environment":"sandbox","collapse_id":"c1","payload":{"aps":{"alert":"hi"}}}`)
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
	reply := host.control(`{"token":"abcd","environment":"sandbox","payload":{}}`)
	if reply.Error == nil || reply.Error.Code != jsonrpc.CodeNotFound {
		t.Fatalf("dead token reply = %v, want not found", reply.Error)
	}
}

func TestPushSendWithoutKeyIsUnavailable(t *testing.T) {
	h := newHarness(t)
	host, _ := h.connect("g", handshake.RoleRuntime)
	reply := host.control(`{"token":"abcd","environment":"sandbox","payload":{}}`)
	if reply.Error == nil || reply.Error.Code != jsonrpc.CodeUnavailable {
		t.Fatalf("keyless relay reply = %v, want unavailable", reply.Error)
	}
}

func TestPushSendIsRateLimited(t *testing.T) {
	var sent atomic.Int32
	h := pushHarness(t, func(context.Context, PushRequest) error { sent.Add(1); return nil })
	host, _ := h.connect("g", handshake.RoleRuntime)
	for i := 0; i < pushBurst; i++ {
		if reply := host.control(`{"token":"t","environment":"sandbox","payload":{}}`); reply.Error != nil {
			t.Fatalf("push %d refused: %v", i, reply.Error)
		}
	}
	if reply := host.control(`{"token":"t","environment":"sandbox","payload":{}}`); reply.Error == nil || reply.Error.Code != jsonrpc.CodeDenied {
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
	host.control(`{"token":"t","environment":"sandbox","payload":{}}`)
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

	host.send(ControlID, `{"jsonrpc":"2.0","id":"1","method":"push.send","params":{"token":"t","environment":"sandbox","payload":{}}}`)
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
