package hostlink_test

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/handshake"
	"github.com/repogo/host/internal/hostclient"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/testhost"
)

type harness struct {
	*testhost.Transports
	devices *device.Store
}

// newHarness attaches a link to a local relay, serving one method.
func newHarness(t *testing.T) *harness {
	t.Helper()
	devices, err := device.Open(filepath.Join(t.TempDir(), "device.json"))
	if err != nil {
		t.Fatalf("devices: %v", err)
	}
	router := rpc.New(slog.New(slog.DiscardHandler))
	rpc.Add(router, "test.ping", func(context.Context, rpc.Caller, rpc.None) (rpc.Ack, error) {
		return rpc.OK, nil
	})
	return &harness{Transports: testhost.Serve(t, devices, router), devices: devices}
}

func (h *harness) dial(t *testing.T, phone *device.Identity, timeout time.Duration) (*hostclient.Client, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, err := hostclient.Dial(ctx, hostclient.Config{
		URL: h.Relay, Identity: phone, GroupID: h.devices.GroupID(),
		Role: handshake.RoleClient, Platform: "ios",
		Host: h.devices.Identity().ID, HostPublic: h.devices.Identity().Public,
	})
	if err == nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return c, err
}

func generate(t *testing.T) *device.Identity {
	t.Helper()
	id, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func denied(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.As(err, &rpcErr) && rpcErr.Code == jsonrpc.CodeDenied
}

func ping(t *testing.T, c *hostclient.Client) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Call(ctx, "test.ping", nil, nil)
}

// A device the host never paired gets no handshake, no reply, and no state:
// anyone can mint a key pair and address the host through the relay.
func TestStrangerIsDroppedBeforeTheHandshake(t *testing.T) {
	h := newHarness(t)
	for range 3 {
		if _, err := h.dial(t, generate(t), 500*time.Millisecond); err == nil {
			t.Fatal("a stranger opened a channel with no pairing code live")
		}
	}
	if n := h.Link.PeerCount(); n != 0 {
		t.Fatalf("host kept %d peers for strangers, want 0", n)
	}
}

// While a code is live a stranger may open a channel, to reach pair.complete,
// and nothing else.
func TestStrangerMayHandshakeWhilePairing(t *testing.T) {
	h := newHarness(t)
	h.Pairing.Store(true)
	c, err := h.dial(t, generate(t), 5*time.Second)
	if err != nil {
		t.Fatalf("a joiner could not open a channel while pairing: %v", err)
	}
	if err := ping(t, c); !denied(err) {
		t.Fatalf("an unpaired joiner called a method: %v, want denied", err)
	}
}

// Revoking ends the device's channel and the calls on it at once, and from
// then on it is a stranger: no pushes, and its redial is dropped.
func TestRevokedDeviceLosesItsChannel(t *testing.T) {
	h := newHarness(t)
	phone := generate(t)
	if err := h.devices.Add(device.Peer{ID: phone.ID, Public: phone.Public, Label: "phone", Platform: "ios"}); err != nil {
		t.Fatalf("pair: %v", err)
	}
	c, err := h.dial(t, phone, 5*time.Second)
	if err != nil {
		t.Fatalf("paired phone: %v", err)
	}
	if err := ping(t, c); err != nil {
		t.Fatalf("paired phone call: %v", err)
	}
	if !h.Link.Online(phone.ID) {
		t.Fatal("a phone with a channel is not online")
	}

	if err := h.devices.Revoke("", phone.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if h.Link.Online(phone.ID) {
		t.Error("a revoked phone's channel outlived the revoke")
	}
	if err := ping(t, c); err == nil {
		t.Error("a revoked phone's open channel still answered")
	}
	if err := h.Link.Send(phone.ID, "test.event", []byte("{}")); err == nil {
		t.Error("the link sealed a push to a revoked phone")
	}
	if _, err := h.dial(t, phone, 500*time.Millisecond); err == nil {
		t.Error("a revoked phone opened a channel again")
	}
}
