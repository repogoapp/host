// Package devices is the `devices.*` methods: who is in the group. Host-side,
// so the relay can never enumerate a user's devices.
package devices

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/rpc"
)

func init() {
	emit.Register(Changed{})
}

type Deps struct {
	Store  *device.Store
	Pairer *device.Pairer
	// Online reports whether a device holds a live channel to this host.
	Online func(device.ID) bool
}

type RegisterPushParams struct {
	device.PushTarget
	// ChatID selects that chat's Live Activity; Kind "start" is the token that
	// starts one. Neither is the device's alert token.
	ChatID string `json:"chat_id"`
	Kind   string `json:"kind"`
}

// Device is one paired device as the Environment screen lists it. Its push
// tokens stay on the host: no phone needs another's.
type Device struct {
	ID        device.ID `json:"id"`
	Label     string    `json:"label"`
	Platform  string    `json:"platform"`
	AddedAt   int64     `json:"added_at"`
	Connected bool      `json:"connected"`
	// You marks the caller's own row without matching a label the user can change.
	You bool `json:"you"`
}

type ListResult struct {
	Devices []Device `json:"devices" wire:"array"`
}

type RevokeParams struct {
	ID device.ID `json:"id"`
}

// Changed tells every paired device its list is stale; each asks again, since
// connected and you differ by the device asking.
type Changed struct{}

func (Changed) Method() string { return "devices.changed" }

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "devices.list", d.list)
	rpc.Add(r, "devices.register_push", d.registerPush, rpc.Paired)
	// Not Paired: the owner at the machine revokes a lost phone from the CLI.
	rpc.Add(r, "devices.revoke", d.revoke, rpc.Detached)
}

func (d Deps) registerPush(_ context.Context, c rpc.Caller, a RegisterPushParams) (rpc.Ack, error) {
	return rpc.OK, d.Store.RegisterPush(c.Device, a.Kind, a.ChatID, a.PushTarget)
}

func (d Deps) list(_ context.Context, c rpc.Caller, _ rpc.None) (ListResult, error) {
	peers := d.Store.Peers()
	slices.SortFunc(peers, func(a, b device.Peer) int { return cmp.Compare(a.AddedAt, b.AddedAt) })
	out := make([]Device, 0, len(peers))
	for _, p := range peers {
		out = append(out, Device{
			ID: p.ID, Label: p.Label, Platform: p.Platform, AddedAt: p.AddedAt,
			Connected: d.Online(p.ID), You: p.ID == c.Device,
		})
	}
	return ListResult{Devices: out}, nil
}

func (d Deps) revoke(_ context.Context, c rpc.Caller, a RevokeParams) (rpc.Ack, error) {
	if _, err := a.ID.Bytes(); err != nil {
		return rpc.Ack{}, fmt.Errorf("%w: id must be a device id", errkind.ErrInvalid)
	}
	return rpc.OK, d.Pairer.Remove(c.Device, a.ID)
}
