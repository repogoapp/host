// Package pairing is the `pair.*` methods: admitting a new device to the group.
package pairing

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/rpc"
)

func init() {
	emit.Register(Completed{})
}

type Deps struct {
	Store  *device.Store
	Pairer *device.Pairer

	// Addr is the address an invite should advertise, resolved late because the
	// listener picks its port after the router is built.
	Addr func() string
	// PairHost is the site pairing links open on and codes are parked at.
	PairHost string
	Log      *slog.Logger
}

// BeginResult is the encoded invite, what the QR and the pairing link carry.
type BeginResult struct {
	QR string `json:"qr"`
}

type StatusResult struct {
	Pending bool `json:"pending"`
}

// InviteParams tags the code so the asking device knows when it is used.
type InviteParams struct {
	InviteID string `json:"invite_id"`
}

// Completed tells every device that the one-time code tagged InviteID was
// used, so the sheet showing it closes.
type Completed struct {
	InviteID string `json:"invite_id"`
}

func (Completed) Method() string { return "pair.completed" }

// InviteResult is a parked one-time code, for a paired device to show another.
type InviteResult struct {
	Code      string `json:"code"`
	Link      string `json:"link"`
	ExpiresAt int64  `json:"expires_at"`
}

type ReusableParams struct {
	Hours int `json:"hours"`
}

type CompleteParams struct {
	DeviceID  string `json:"device_id"`
	PublicKey []byte `json:"public_key"`
	Label     string `json:"label"`
	Platform  string `json:"platform"`
	Proof     []byte `json:"proof"`
}

type CompleteResult struct {
	HostID     device.ID `json:"host_id"`
	HostPublic []byte    `json:"host_public"`
	HostProof  []byte    `json:"host_proof"`
	// HostSignature is the host identity's signature over the joiner's key and
	// the TLS pin it dialed (device.PairMessage); empty without a relay.
	HostSignature []byte `json:"host_signature,omitempty"`
	// Sent here because the client saves the pairing before it ever connects
	// as a paired device.
	HostLabel string `json:"host_label"`
}

func Register(r *rpc.Router, d Deps) {
	// begin mints the code that admits a new device to the group, for the CLI
	// at the machine, which parks it on the pair host itself.
	rpc.Add(r, "pair.begin", d.begin, rpc.Local)
	rpc.Add(r, "pair.status", d.status, rpc.Local)
	// invite lets a paired device pair another: holding a paired key already
	// reaches a shell here, so being at the machine adds nothing over it.
	rpc.Add(r, "pair.invite", d.invite)
	rpc.Add(r, "pair.reusable", d.reusable, rpc.Local)
	rpc.Add(r, "pair.reusable_revoke", d.reusableRevoke, rpc.Local)

	// complete is the one method an unpaired caller may reach — the bootstrap
	// paradox. It is guarded by the pairing code instead.
	rpc.Add(r, "pair.complete", d.complete, rpc.Unpaired)
}

// begin issues an invite. The QR carries the raw code because the caller is
// the local UI that renders it — the response is never sent to the joiner.
func (d Deps) begin(context.Context, rpc.Caller, rpc.None) (BeginResult, error) {
	invite, err := d.Pairer.Begin(d.Addr(), "")
	if err != nil {
		return BeginResult{}, err
	}
	encoded, err := invite.Encode()
	return BeginResult{QR: encoded}, err
}

// invite issues a one-time invite, as begin does, parked on the pair host so
// the caller gets the short code and link rather than the raw invite.
func (d Deps) invite(_ context.Context, c rpc.Caller, a InviteParams) (InviteResult, error) {
	// Echoed to every device, so kept short.
	if a.InviteID == "" || len(a.InviteID) > 64 {
		return InviteResult{}, errkind.New(errkind.Invalid, "invite_id must be 1 to 64 characters")
	}
	invite, err := d.Pairer.Begin(d.Addr(), a.InviteID)
	if err != nil {
		return InviteResult{}, err
	}
	encoded, err := invite.Encode()
	if err != nil {
		return InviteResult{}, err
	}
	code, err := device.ParkInvite(d.PairHost, encoded, invite, false)
	if err != nil {
		d.Pairer.Cancel(invite.Code)
		return InviteResult{}, fmt.Errorf("park the pairing code at %s: %w", d.PairHost, err)
	}
	d.Log.Info("pairing: invite issued", "by", c.Device)
	return InviteResult{Code: code, Link: device.CodeLink(d.PairHost, code), ExpiresAt: invite.ExpiresAt}, nil
}

// reusable issues an invite many devices may join with for a.Hours, for a
// machine nobody stands at to show a fresh code.
func (d Deps) reusable(_ context.Context, _ rpc.Caller, a ReusableParams) (BeginResult, error) {
	invite, err := d.Pairer.BeginReusable(d.Addr(), time.Duration(a.Hours)*time.Hour)
	if err != nil {
		return BeginResult{}, err
	}
	encoded, err := invite.Encode()
	return BeginResult{QR: encoded}, err
}

func (d Deps) reusableRevoke(context.Context, rpc.Caller, rpc.None) (rpc.Ack, error) {
	return rpc.OK, d.Pairer.RevokeReusable()
}

func (d Deps) status(context.Context, rpc.Caller, rpc.None) (StatusResult, error) {
	pending, _ := d.Pairer.Pending()
	return StatusResult{Pending: pending}, nil
}

// complete admits a device that proves it saw the QR: an HMAC over its public
// key, keyed by a pairing code. Nothing is written before that check.
func (d Deps) complete(_ context.Context, _ rpc.Caller, a CompleteParams) (CompleteResult, error) {
	joiner := device.Peer{
		ID:     device.ID(a.DeviceID),
		Public: a.PublicKey,
		Label:  a.Label,
		Client: device.Client{Platform: a.Platform},
	}
	hostProof, err := d.Pairer.Complete(joiner, a.Proof)
	if err != nil {
		return CompleteResult{}, err
	}

	self := d.Store.Identity()
	var signature []byte
	if pin := d.Pairer.TLSPin(); pin != nil {
		if signature, err = self.SignPair(joiner.Public, *pin); err != nil {
			return CompleteResult{}, err
		}
	}
	// The host's own proof goes back so the joiner can confirm it paired with
	// the machine whose screen it scanned, rather than something in the middle.
	return CompleteResult{
		HostID:        self.ID,
		HostPublic:    self.Public,
		HostProof:     hostProof,
		HostSignature: signature,
		HostLabel:     device.Label(),
	}, nil
}
