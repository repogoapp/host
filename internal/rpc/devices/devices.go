// Package devices is the `devices.*` methods: who is in the group. Host-side,
// so the relay can never enumerate a user's devices.
package devices

import (
	"context"
	"encoding/base64"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/rpc"
)

type Deps struct {
	Store *device.Store
}

type RegisterPushParams struct {
	device.PushTarget
	// ChatID selects that chat's Live Activity; Kind "start" is the token that
	// starts one. Neither is the device's alert token.
	ChatID string `json:"chat_id"`
	Kind   string `json:"kind"`
}

type Self struct {
	ID device.ID `json:"id"`
	// The machine's own name, so a client paired with two Macs can tell them apart.
	Label     string `json:"label"`
	Platform  string `json:"platform"`
	PublicKey string `json:"public_key"`
}

type ListResult struct {
	GroupID string `json:"group_id"`
	Self    Self   `json:"self"`
	// You lets a client mark its own row without string-matching a label the
	// user can change.
	You   device.ID     `json:"you"`
	Peers []device.Peer `json:"peers" wire:"array"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "devices.list", d.list)
	rpc.Add(r, "devices.register_push", d.registerPush, rpc.Paired)
}

func (d Deps) registerPush(_ context.Context, c rpc.Caller, a RegisterPushParams) (rpc.Ack, error) {
	return rpc.OK, d.Store.RegisterPush(c.Device, a.Kind, a.ChatID, a.PushTarget)
}

func (d Deps) list(_ context.Context, c rpc.Caller, _ rpc.None) (ListResult, error) {
	self := d.Store.Identity()
	return ListResult{
		GroupID: d.Store.GroupID(),
		Self: Self{
			ID:        self.ID,
			Label:     device.Label(),
			Platform:  "host",
			PublicKey: base64.StdEncoding.EncodeToString(self.Public),
		},
		You:   c.Device,
		Peers: d.Store.ActivePeers(),
	}, nil
}
