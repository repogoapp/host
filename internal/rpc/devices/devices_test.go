package devices_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/handshake"
	"github.com/repogo/host/internal/host"
	"github.com/repogo/host/internal/hostclient"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/rpc/devices"
	"github.com/repogo/host/internal/rpc/pairing"
	"github.com/repogo/host/internal/testhost"
	"github.com/repogo/host/internal/testwait"
)

// running is the whole host, listening on loopback and attached to a local
// relay, so revoking runs through the same wiring the host ships with.
type running struct {
	*testhost.Host
	ws, relay string
}

func start(t *testing.T) *running {
	t.Helper()
	relay := testhost.Relay(t)
	th := testhost.New(t, func(c *host.Config) { c.Relay = relay })
	addr, err := th.Listen()
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	th.Start()
	testwait.For(t, "the host to attach to the relay", func() bool {
		s, err := th.Config.Host.Status(t.Context())
		return err == nil && s.RelayConnected
	})
	return &running{Host: th, ws: fmt.Sprintf("ws://%s/ws", addr), relay: relay}
}

// local calls the router as the CLI does: from this machine, as the host.
func (h *running) local(t *testing.T, method string, params, into any) error {
	t.Helper()
	b, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	out, err := h.Router.Call(t.Context(), rpc.Caller{Device: h.Devices.Identity().ID, Scope: rpc.ScopeLocal}, method, b)
	if err != nil || into == nil {
		return err
	}
	return json.Unmarshal(out, into)
}

func (h *running) overRelay(t *testing.T, phone *device.Identity, timeout time.Duration) (*hostclient.Client, error) {
	t.Helper()
	return h.dial(t, timeout, hostclient.Config{
		URL: h.relay, Identity: phone, GroupID: h.Devices.GroupID(), Role: handshake.RoleClient, Platform: "ios",
		Host: h.Devices.Identity().ID, HostPublic: h.Devices.Identity().Public,
	})
}

func (h *running) overLoopback(t *testing.T, phone *device.Identity) (*hostclient.Client, error) {
	t.Helper()
	return h.dial(t, 5*time.Second, hostclient.Config{
		URL: h.ws, Identity: phone, GroupID: h.Devices.GroupID(), Role: handshake.RoleClient, Platform: "ios",
	})
}

func (h *running) dial(t *testing.T, timeout time.Duration, cfg hostclient.Config) (*hostclient.Client, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	c, err := hostclient.Dial(ctx, cfg)
	if err == nil {
		t.Cleanup(func() { _ = c.Close() })
	}
	return c, err
}

func call(t *testing.T, c *hostclient.Client, method string, params, into any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	return c.Call(ctx, method, params, into)
}

func generate(t *testing.T) *device.Identity {
	t.Helper()
	id, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func code(err error) int {
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) {
		return rpcErr.Code
	}
	return 0
}

// One phone removes another that joined with the reusable invite: the removed
// phone loses its channel and its socket, cannot come back with the invite or
// its key, and the phone that removed it hears the list changed.
func TestRevokeRemovesAPhoneEverywhere(t *testing.T) {
	h := start(t)
	keeper, lost := generate(t), generate(t)
	if err := h.Devices.Add(device.Peer{ID: keeper.ID, Public: keeper.Public, Label: "Keeper", Platform: "ios"}); err != nil {
		t.Fatalf("pair keeper: %v", err)
	}

	var invite pairing.BeginResult
	if err := h.local(t, "pair.reusable", pairing.ReusableParams{Hours: 24}, &invite); err != nil {
		t.Fatalf("reusable invite: %v", err)
	}
	join := pairing.CompleteParams{
		DeviceID: string(lost.ID), PublicKey: lost.Public, Label: "Lost", Platform: "ios",
		Proof: device.Proof(invite.Invite.Code, lost.Public),
	}
	if err := h.local(t, "pair.complete", join, nil); err != nil {
		t.Fatalf("lost phone joins with the invite: %v", err)
	}

	k, err := h.overRelay(t, keeper, 5*time.Second)
	if err != nil {
		t.Fatalf("keeper over the relay: %v", err)
	}
	lostRelay, err := h.overRelay(t, lost, 5*time.Second)
	if err != nil {
		t.Fatalf("lost phone over the relay: %v", err)
	}
	lostLocal, err := h.overLoopback(t, lost)
	if err != nil {
		t.Fatalf("lost phone over loopback: %v", err)
	}
	push := devices.RegisterPushParams{PushTarget: lost.GrantPush(h.Devices.Identity().ID, device.PushTarget{Token: "abcd", Environment: "sandbox"})}
	if err := call(t, lostRelay, "devices.register_push", push, nil); err != nil {
		t.Fatalf("lost phone registers push: %v", err)
	}

	var before devices.ListResult
	if err := call(t, k, "devices.list", nil, &before); err != nil {
		t.Fatalf("devices.list: %v", err)
	}
	if len(before.Devices) != 2 || !slices.ContainsFunc(before.Devices, func(d devices.Device) bool {
		return d.ID == lost.ID && d.Connected && !d.You
	}) {
		t.Fatalf("before revoking, list = %+v, want the lost phone connected", before.Devices)
	}

	if err := call(t, k, "devices.revoke", devices.RevokeParams{ID: lost.ID}, nil); err != nil {
		t.Fatalf("devices.revoke: %v", err)
	}

	// (b) Both open connections fail their next call.
	if err := call(t, lostRelay, "devices.list", nil, nil); err == nil {
		t.Error("the lost phone's relay channel still answered")
	}
	if err := call(t, lostLocal, "devices.list", nil, nil); err == nil {
		t.Error("the lost phone's loopback socket still answered")
	}

	// (a) Its hello is refused as unknown; through the relay it is a stranger, dropped unanswered.
	if _, err := h.overLoopback(t, lost); err == nil || !strings.Contains(err.Error(), device.ErrUnknownDevice.Error()) {
		t.Errorf("loopback hello after revoke: %v, want refused as an unknown device", err)
	}
	if _, err := h.overRelay(t, lost, 500*time.Millisecond); err == nil {
		t.Error("the lost phone opened a relay channel after revoke")
	}

	// (c) The invite it held is over.
	if err := h.local(t, "pair.complete", join, nil); !errors.Is(err, device.ErrNoPairing) {
		t.Errorf("pair.complete with the held invite: %v, want no pairing", err)
	}

	// (d) The list is the keeper alone, in the screen's fields and nothing else.
	var raw json.RawMessage
	if err := call(t, k, "devices.list", nil, &raw); err != nil {
		t.Fatalf("devices.list: %v", err)
	}
	var after struct {
		Devices []map[string]json.RawMessage `json:"devices"`
	}
	if err := json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Devices) != 1 || string(after.Devices[0]["id"]) != `"`+string(keeper.ID)+`"` {
		t.Fatalf("after revoking, list = %s, want the keeper alone", raw)
	}
	var keys []string
	for k := range after.Devices[0] {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"added_at", "connected", "id", "label", "platform", "you"}; !slices.Equal(keys, want) {
		t.Errorf("a listed device carries %v, want only %v", keys, want)
	}
	if strings.Contains(string(raw), "abcd") || strings.Contains(string(raw), "token") {
		t.Errorf("devices.list leaks push tokens: %s", raw)
	}

	// (e) The keeper hears it.
	timeout := time.After(testwait.Timeout)
	for heard := false; !heard; {
		select {
		case n, ok := <-k.Notifications:
			if !ok {
				t.Fatal("the keeper's connection closed before devices.changed")
			}
			heard = n.Method == "devices.changed"
		case <-timeout:
			t.Fatal("the keeper never heard devices.changed")
		}
	}
}

// (f) A phone removes itself by forgetting the host, and nobody removes the host.
func TestRevokeRefusesSelfAndHost(t *testing.T) {
	h := start(t)
	phone := generate(t)
	if err := h.Devices.Add(device.Peer{ID: phone.ID, Public: phone.Public, Label: "Phone", Platform: "ios"}); err != nil {
		t.Fatalf("pair: %v", err)
	}
	c, err := h.overRelay(t, phone, 5*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	for _, id := range []device.ID{phone.ID, h.Devices.Identity().ID, "not-an-id"} {
		if err := call(t, c, "devices.revoke", devices.RevokeParams{ID: id}, nil); code(err) != jsonrpc.CodeInvalidParams {
			t.Errorf("revoking %s from the phone: %v, want invalid", id, err)
		}
	}
	if err := h.local(t, "devices.revoke", devices.RevokeParams{ID: h.Devices.Identity().ID}, nil); !errors.Is(err, rpc.ErrInvalidParams) {
		t.Errorf("revoking the host from the machine: %v, want invalid", err)
	}
	if _, err := h.Devices.Peer(phone.ID); err != nil {
		t.Errorf("a refused revoke removed the phone: %v", err)
	}
}
