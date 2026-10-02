// Package tunnels is the `tunnels.*` methods: the public URLs this host serves
// for its own dev servers; the serving lives in internal/tunnel.
package tunnels

import (
	"context"

	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/tunnel"
)

type Deps struct {
	Tunnels *tunnel.Service
}

type SlugParams struct {
	Slug string `json:"slug"`
}

type OpenParams struct {
	SlugParams
	Port      int   `json:"port"`
	ExpiresAt int64 `json:"expires_at"`
}

type OpenResult struct {
	Tunnel tunnel.Tunnel `json:"tunnel"`
}

func Register(r *rpc.Router, d Deps) {
	// open allows a slug repogo.app granted to reach one port. A paired device
	// only: the tunnel lasts while that device stays paired.
	rpc.Add(r, "tunnels.open", d.open, rpc.Paired)
	rpc.Add(r, "tunnels.close", d.close)
	rpc.Add(r, "tunnels.list", d.list)
}

func (d Deps) open(_ context.Context, c rpc.Caller, a OpenParams) (OpenResult, error) {
	t, err := d.Tunnels.Open(c.Device, a.Slug, a.Port, a.ExpiresAt)
	return OpenResult{Tunnel: t}, err
}

func (d Deps) close(_ context.Context, _ rpc.Caller, a SlugParams) (rpc.Ack, error) {
	return rpc.OK, d.Tunnels.Close(a.Slug)
}

func (d Deps) list(context.Context, rpc.Caller, rpc.None) (tunnel.Changed, error) {
	return d.Tunnels.Status(), nil
}
