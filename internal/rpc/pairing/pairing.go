// Package pairing is the `pair.*` methods: admitting a new device to the group.
package pairing

import (
	"context"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/rpc"
)

type Deps struct {
	Store  *device.Store
	Pairer *device.Pairer

	// Addr is the address an invite should advertise, resolved late because the
	// listener picks its port after the router is built.
	Addr func() string
}

type BeginResult struct {
	Invite device.Invite `json:"invite"`
	QR     string        `json:"qr"`
}

type StatusResult struct {
	Pending   bool  `json:"pending"`
	ExpiresAt int64 `json:"expires_at,omitempty"`
}

type CompleteParams struct {
	DeviceID  string `json:"device_id"`
	PublicKey []byte `json:"public_key"`
	Label     string `json:"label"`
	Platform  string `json:"platform"`
	Proof     []byte `json:"proof"`
}

type CompleteResult struct {
	GroupID    string    `json:"group_id"`
	HostID     device.ID `json:"host_id"`
	HostPublic []byte    `json:"host_public"`
	HostProof  []byte    `json:"host_proof"`
	// Sent here because the client saves the pairing before it ever connects
	// as a paired device.
	HostLabel string `json:"host_label"`
}

func Register(r *rpc.Router, d Deps) {
	// begin mints the code that admits a new device to the group. Requiring
	// physical presence at the machine is the entire security model of the QR.
	rpc.Add(r, "pair.begin", d.begin, rpc.Local)
	rpc.Add(r, "pair.status", d.status, rpc.Local)

	// complete is the one method an unpaired caller may reach — the bootstrap
	// paradox. It is guarded by the pairing code instead.
	rpc.Add(r, "pair.complete", d.complete, rpc.Unpaired)
}

// begin issues an invite. The response carries the raw code because the caller
// is the local UI that renders the QR — it is never sent to the joiner.
func (d Deps) begin(context.Context, rpc.Caller, rpc.None) (BeginResult, error) {
	invite, err := d.Pairer.Begin(d.Addr())
	if err != nil {
		return BeginResult{}, err
	}
	encoded, err := invite.Encode()
	return BeginResult{Invite: invite, QR: encoded}, err
}

func (d Deps) status(context.Context, rpc.Caller, rpc.None) (StatusResult, error) {
	pending, expires := d.Pairer.Pending()
	out := StatusResult{Pending: pending}
	if pending {
		out.ExpiresAt = expires.UnixMilli()
	}
	return out, nil
}

// complete admits a device that proves it saw the QR: an HMAC over its public
// key, keyed by the short-lived code. Nothing is written before that check.
func (d Deps) complete(_ context.Context, _ rpc.Caller, a CompleteParams) (CompleteResult, error) {
	joiner := device.Peer{
		ID:       device.ID(a.DeviceID),
		Public:   a.PublicKey,
		Label:    a.Label,
		Platform: a.Platform,
	}
	hostProof, err := d.Pairer.Complete(joiner, a.Proof)
	if err != nil {
		return CompleteResult{}, err
	}

	self := d.Store.Identity()
	// The host's own proof goes back so the joiner can confirm it paired with
	// the machine whose screen it scanned, rather than something in the middle.
	return CompleteResult{
		GroupID:    d.Store.GroupID(),
		HostID:     self.ID,
		HostPublic: self.Public,
		HostProof:  hostProof,
		HostLabel:  device.Label(),
	}, nil
}
