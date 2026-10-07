package wsserver_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/hostclient"
	"github.com/repogo/host/internal/rpc/devices"
	"github.com/repogo/host/internal/rpc/registry"
	"github.com/repogo/host/internal/testhost"
)

// These exercise the full client conversation — challenge, signature, RPC,
// revocation — against a real server over a real socket, in process, so no
// running host or paired phone is needed.

type harness struct {
	t       *testing.T
	url     string
	devices *device.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	th := testhost.New(t)
	router, err := registry.New(th.Config)
	if err != nil {
		t.Fatalf("new registry: %v", err)
	}
	return &harness{t: t, url: testhost.Serve(t, th.Devices, router).WS, devices: th.Devices}
}

// pair admits a device the way a completed QR handshake would, without the
// round trip — this file is about what happens on the socket afterwards.
func (h *harness) pair(label string) *device.Identity {
	h.t.Helper()
	id, err := device.Generate()
	if err != nil {
		h.t.Fatalf("generate identity: %v", err)
	}
	if err := h.devices.Add(device.Peer{
		ID: id.ID, Public: id.Public, Label: label, Platform: "ios",
	}); err != nil {
		h.t.Fatalf("pair %s: %v", label, err)
	}
	return id
}

// dial connects and handshakes, or reports why it was refused. Both are
// legitimate outcomes; which one is the assertion.
func (h *harness) dial(identity *device.Identity, opts ...func(*hostclient.Config)) (*hostclient.Client, error) {
	h.t.Helper()
	cfg := hostclient.Config{
		URL:      h.url,
		Identity: identity,
		GroupID:  h.devices.GroupID(),
		Platform: "ios",
		Label:    "Test phone",
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := hostclient.Dial(ctx, cfg)
	if err == nil {
		h.t.Cleanup(func() { _ = c.Close() })
	}
	return c, err
}

func (h *harness) mustDial(identity *device.Identity) *hostclient.Client {
	h.t.Helper()
	c, err := h.dial(identity)
	if err != nil {
		h.t.Fatalf("connect: %v", err)
	}
	return c
}

func call(t *testing.T, c *hostclient.Client, method string, params, into any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Call(ctx, method, params, into)
}

// --- the cases ---------------------------------------------------------------

func TestPairedDeviceConnectsBySignature(t *testing.T) {
	h := newHarness(t)
	c := h.mustDial(h.pair("Phone"))

	if c.ServerVersion() == "" {
		// Empty here makes a stale host indistinguishable from a fresh one.
		t.Error("the handshake carried no server version")
	}
}

func TestUnpairedDeviceIsRejected(t *testing.T) {
	h := newHarness(t)
	stranger, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}

	// A perfectly valid signature over the right bytes — from a device nobody
	// ever admitted. Signing correctly must not be the same as being allowed in.
	if _, err := h.dial(stranger); err == nil {
		t.Fatal("an unpaired device was accepted")
	}
}

// Revoking closes the device's open socket, so its next call fails, and its
// reconnect is refused as an unknown device.
func TestRevokedDeviceLosesItsSocket(t *testing.T) {
	h := newHarness(t)
	phone := h.pair("Old phone")
	c := h.mustDial(phone)

	if err := h.devices.Revoke("", phone.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if err := call(t, c, "devices.list", nil, nil); err == nil {
		t.Error("a revoked device's open socket still answered")
	}
	if _, err := h.dial(phone); err == nil || !strings.Contains(err.Error(), "unknown device") {
		t.Fatalf("a revoked device reconnected or was refused for another reason: %v", err)
	}
}

func TestSignatureMismatchIsRejected(t *testing.T) {
	h := newHarness(t)
	phone := h.pair("Phone")
	attacker, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}

	// The paired device's id, signed by someone else's key — what possessing a
	// stolen device record but not its private key looks like.
	_, err = h.dial(phone, func(cfg *hostclient.Config) {
		cfg.SignAs = attacker
	})
	if err == nil {
		t.Fatal("accepted a signature made with the wrong key")
	}
	// And the paired device still works, so the refusal was about the proof.
	h.mustDial(phone)
}

func TestPairedDeviceMayNotFallBackToTheToken(t *testing.T) {
	h := newHarness(t)
	phone := h.pair("Phone")

	// Presenting the loopback token while claiming a paired identity would let
	// anything that can read the 0600 file impersonate a phone.
	_, err := h.dial(phone, func(cfg *hostclient.Config) {
		cfg.Unsigned = true
		cfg.LocalToken = testhost.Token
	})
	if err == nil {
		t.Fatal("a paired device authenticated with the token instead of its key")
	}
}

func TestDevicesListLeavesOutRemovedDevices(t *testing.T) {
	h := newHarness(t)
	phone := h.pair("Phone")
	gone := h.pair("Retired phone")
	if err := h.devices.Revoke("", gone.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	c := h.mustDial(phone)

	var got devices.ListResult
	if err := call(t, c, "devices.list", nil, &got); err != nil {
		t.Fatalf("devices.list: %v", err)
	}

	if len(got.Devices) != 1 {
		t.Fatalf("want 1 device, got %d", len(got.Devices))
	}
	if d := got.Devices[0]; d.Label != "Phone" || !d.You {
		t.Errorf("listed %+v, want the calling phone marked as you", d)
	}
}

func TestPingRoundTrip(t *testing.T) {
	h := newHarness(t)
	c := h.mustDial(h.pair("Phone"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

// A request past the WebSocket library's 32 KiB default still gets its answer.
func TestALargeRequestIsAnswered(t *testing.T) {
	h := newHarness(t)
	c := h.mustDial(h.pair("Phone"))
	pad := map[string]string{"pad": strings.Repeat("x", 64<<10)}
	if err := call(t, c, "devices.list", pad, nil); err != nil {
		t.Fatalf("a 64 KiB request: %v", err)
	}
}
