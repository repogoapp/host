// Package hosts is the `hosts.*` methods: linking this host to a RepoGo
// account so repogo.app can tell whose machine it is.
package hosts

import (
	"context"

	"github.com/repogo/host/internal/account"
	"github.com/repogo/host/internal/rpc"
)

type Deps struct {
	Account *account.Service
}

type ClaimParams struct {
	UID   string `json:"uid"`
	Nonce string `json:"nonce"`
}

func Register(r *rpc.Router, d Deps) {
	// claim signs repogo.app's nonce for the phone's signed-in account. A
	// paired device only: the phone is the one holding that account.
	rpc.Add(r, "hosts.claim", d.claim, rpc.Paired)
	// release lets another account claim this host; only at the machine.
	rpc.Add(r, "hosts.release", d.release, rpc.Local)
}

func (d Deps) claim(_ context.Context, _ rpc.Caller, a ClaimParams) (account.Proof, error) {
	return d.Account.Claim(a.UID, a.Nonce)
}

func (d Deps) release(context.Context, rpc.Caller, rpc.None) (rpc.Ack, error) {
	return rpc.OK, d.Account.Release()
}
