// Package host is the `host.*` methods: this machine's status, its setup
// screen, updating the host itself, and linking it to a RepoGo account so
// repogo.app can tell whose machine it is.
package host

import (
	"context"
	"time"

	"github.com/repogo/host/internal/account"
	"github.com/repogo/host/internal/hostinfo"
	"github.com/repogo/host/internal/hostsetup"
	"github.com/repogo/host/internal/hostupdate"
	"github.com/repogo/host/internal/rpc"
)

type Deps struct {
	Service *hostinfo.Service
	Updates *hostupdate.Updater
	Account *account.Service

	// Setup is this host's release, GitHub, the agent CLIs and what each offers.
	Setup hostsetup.Sources
}

// SetupParams' refresh re-reads every agent's catalog first, for a picker's
// pull to refresh.
type SetupParams struct {
	Refresh bool `json:"refresh"`
}

type SetStopsAtParams struct {
	StopsAt time.Time `json:"stops_at"`
}

type ClaimParams struct {
	UID   string `json:"uid"`
	Nonce string `json:"nonce"`
}

type UpdateParams struct {
	// Now stops running chats, terminals and actions rather than refusing.
	Now bool `json:"now"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "host.status", d.status)
	// setup is everything an environment's screens ask: this host's release,
	// whose GitHub, which agents are ready, and every model each offers.
	rpc.Add(r, "host.setup", d.setup)
	// Detached: a binary swapped with no restart after it is worse than either.
	rpc.Add(r, "host.update", d.update, rpc.Detached)
	// A device extended this cloud host's session with its provider.
	rpc.Add(r, "host.set_stops_at", d.setStopsAt)
	// claim signs repogo.app's nonce for the phone's signed-in account. A
	// paired device only: the phone is the one holding that account.
	rpc.Add(r, "host.claim", d.claim, rpc.Paired)
	// release lets another account claim this host; only at the machine.
	rpc.Add(r, "host.release", d.release, rpc.Local)
}

func (d Deps) status(ctx context.Context, _ rpc.Caller, _ rpc.None) (hostinfo.Status, error) {
	return d.Service.Status(ctx)
}

func (d Deps) setup(ctx context.Context, _ rpc.Caller, a SetupParams) (hostsetup.Setup, error) {
	return hostsetup.Snapshot(ctx, d.Setup, a.Refresh), nil
}

func (d Deps) setStopsAt(_ context.Context, _ rpc.Caller, a SetStopsAtParams) (hostinfo.Cloud, error) {
	return d.Service.SetStopsAt(a.StopsAt)
}

func (d Deps) update(ctx context.Context, _ rpc.Caller, a UpdateParams) (hostupdate.Result, error) {
	return d.Updates.Update(ctx, a.Now)
}

func (d Deps) claim(_ context.Context, _ rpc.Caller, a ClaimParams) (account.Proof, error) {
	return d.Account.Claim(a.UID, a.Nonce)
}

func (d Deps) release(context.Context, rpc.Caller, rpc.None) (rpc.Ack, error) {
	return rpc.OK, d.Account.Release()
}
