// Package host is the `host.*` methods: this machine's status, its setup
// screen and updating the host itself.
package host

import (
	"context"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/errkind"

	"github.com/repogoapp/host/internal/hostinfo"
	"github.com/repogoapp/host/internal/hostsetup"
	"github.com/repogoapp/host/internal/hostupdate"
	"github.com/repogoapp/host/internal/rpc"
)

type Deps struct {
	Service *hostinfo.Service
	Updates *hostupdate.Updater

	// Setup is this host's release, GitHub, the agent CLIs and what each offers.
	Setup hostsetup.Sources

	// TLSRotate replaces the TLS key phones pin; nil when there is none.
	TLSRotate func() (device.SignedPin, error)
}

// SetupParams' refresh re-reads every agent's catalog first, for a picker's
// pull to refresh.
type SetupParams struct {
	Refresh bool `json:"refresh"`
}

// RenameParams names this host for every device; "" goes back to the
// machine's own name.
type RenameParams struct {
	Name string `json:"name"`
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
	// rename names this host for every device, and they hear host.name_changed.
	rpc.Add(r, "host.rename", d.rename)
	// Detached: a binary swapped with no restart after it is worse than either.
	rpc.Add(r, "host.update", d.update, rpc.Detached)
	// Local: only someone at the machine replaces its key (repogo tls rotate).
	rpc.Add(r, "host.tls_rotate", d.tlsRotate, rpc.Local)
}

func (d Deps) tlsRotate(context.Context, rpc.Caller, rpc.None) (device.SignedPin, error) {
	if d.TLSRotate == nil {
		return device.SignedPin{}, errkind.New(errkind.NotFound, "this host has no relay, so no TLS key")
	}
	return d.TLSRotate()
}

func (d Deps) rename(_ context.Context, _ rpc.Caller, a RenameParams) (hostinfo.Named, error) {
	return d.Service.Rename(a.Name)
}

func (d Deps) status(context.Context, rpc.Caller, rpc.None) (hostinfo.Status, error) {
	return d.Service.Status(), nil
}

func (d Deps) setup(ctx context.Context, _ rpc.Caller, a SetupParams) (hostsetup.Setup, error) {
	return hostsetup.Snapshot(ctx, d.Setup, a.Refresh), nil
}

func (d Deps) update(ctx context.Context, _ rpc.Caller, a UpdateParams) (hostupdate.Result, error) {
	return d.Updates.Update(ctx, a.Now)
}
