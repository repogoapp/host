// Package shipping is the `usage.history` method: what this machine's agents
// spent, for the phone's usage screen. Only counts and costs, never content.
package shipping

import (
	"context"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/rpc"
	shippingcore "github.com/repogoapp/host/internal/shipping"
)

type Deps struct {
	Usage *shippingcore.Ledger
	// ModelLabel names each bucket's model for the phone.
	ModelLabel func(agent.Kind, string) string
	// Checkouts is this environment's project folders with a first commit:
	// every checkout of one repository.
	Checkouts func(rootCommit string) ([]string, error)
}

func Register(r *rpc.Router, d Deps) {
	// Remote: the phone adds every environment's answer into one screen.
	rpc.Add(r, "usage.history", d.history)
}

// HistoryParams is a range of Unix milliseconds, until exclusive.
type HistoryParams struct {
	SinceMs int64 `json:"since_ms"`
	UntilMs int64 `json:"until_ms"`
	// Cwd narrows the answer to one project: requests run in this folder or
	// under it. Empty is the whole environment.
	Cwd string `json:"cwd,omitempty"`
	// RootCommit narrows it to one repository: every checkout here whose first
	// commit this is, and the folders under them. Wins over Cwd. A repository
	// with no checkout here answers with nothing.
	RootCommit string `json:"root_commit,omitempty"`
}

func (d Deps) history(ctx context.Context, _ rpc.Caller, p HistoryParams) (shippingcore.History, error) {
	scope, err := d.scope(p)
	if err != nil {
		return shippingcore.History{}, err
	}
	h, err := d.Usage.History(ctx, p.SinceMs, p.UntilMs, scope)
	for i, b := range h.Buckets {
		h.Buckets[i].ModelLabel = d.ModelLabel(agent.Kind(b.Agent), b.Model)
	}
	return h, err
}

// scope is the requests a call asks about: one repository's checkouts, one
// folder, or everything.
func (d Deps) scope(p HistoryParams) (shippingcore.Scope, error) {
	switch {
	case p.RootCommit != "":
		paths, err := d.Checkouts(p.RootCommit)
		return shippingcore.Folders(paths...), err
	case p.Cwd != "":
		return shippingcore.Folders(p.Cwd), nil
	default:
		return shippingcore.Scope{}, nil
	}
}
