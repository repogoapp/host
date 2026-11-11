// Package devices is the `devices.*` methods: who is in the group. Host-side,
// so the relay can never enumerate a user's devices.
package devices

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/rpc"
)

func init() {
	emit.Register(Changed{})
}

type Deps struct {
	Store  *device.Store
	Pairer *device.Pairer
	// Online reports whether a device holds a live channel to this host.
	Online func(device.ID) bool
	// PushChanged hears each token a phone registers or clears, by kind.
	PushChanged func(kind string)
}

// HelloParams is what the phone says it is, on every connect.
type HelloParams struct {
	device.Client
}

// RegisterPushParams is a push token and its grant; the host stamps when it
// first saw the token itself.
type RegisterPushParams struct {
	Token string `json:"token"`
	// Service is "apns" or "fcm"; an empty token, which forgets, needs none.
	Service     string `json:"service"`
	Environment string `json:"environment"`
	InstallID   string `json:"install_id,omitempty"`
	Grant       []byte `json:"grant,omitempty"`
	GrantedAt   int64  `json:"granted_at,omitempty"`
	// Kind "start" is the token that starts the Live Activity and "activity"
	// the running one's; "steps_start" and "steps" are a plugin's step
	// activity's. Empty is the device's alert token.
	Kind string `json:"kind"`
	// Activity is the step activity a "steps" token updates.
	Activity string `json:"activity,omitempty"`
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
	// The phone's first call on each connect: an update changes its version
	// and features, which pick the prompt its turns get.
	rpc.Add(r, "devices.hello", d.hello, rpc.Paired)
	rpc.Add(r, "devices.register_push", d.registerPush, rpc.Paired)
	// Not Paired: the owner at the machine revokes a lost phone from the CLI.
	rpc.Add(r, "devices.revoke", d.revoke, rpc.Detached)
}

func (d Deps) hello(_ context.Context, c rpc.Caller, a HelloParams) (rpc.Ack, error) {
	return rpc.OK, d.Store.SetClient(c.Device, a.Client)
}

func (d Deps) registerPush(_ context.Context, c rpc.Caller, a RegisterPushParams) (rpc.Ack, error) {
	target := device.PushTarget{
		Token: a.Token, Service: a.Service, Environment: a.Environment, InstallID: a.InstallID,
		Grant: a.Grant, GrantedAt: a.GrantedAt,
	}
	var err error
	if a.Kind == device.PushSteps {
		err = d.Store.RegisterStepsPush(c.Device, a.Activity, target)
	} else {
		err = d.Store.RegisterPush(c.Device, a.Kind, target)
	}
	if err != nil {
		return rpc.Ack{}, err
	}
	if d.PushChanged != nil {
		d.PushChanged(a.Kind)
	}
	return rpc.OK, nil
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
