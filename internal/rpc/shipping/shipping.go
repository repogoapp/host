// Package shipping is the `usage.daily` and `usage.history` methods:
// what this machine's agents spent, for the phone's usage screen and the
// leaderboard it reports to. Only counts and costs, never content.
package shipping

import (
	"context"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/rpc"
	shippingcore "github.com/repogo/host/internal/shipping"
)

type Deps struct {
	Usage *shippingcore.Ledger
	// ModelLabel names each bucket's model for the phone.
	ModelLabel func(agent.Kind, string) string
}

func Register(r *rpc.Router, d Deps) {
	// Remote: a paired phone uploads it; only counts and costs, never content.
	rpc.Add(r, "usage.daily", d.usage)
	// Remote: the phone adds every environment's answer into one screen.
	rpc.Add(r, "usage.history", d.history)
}

// HistoryParams is a range of Unix milliseconds, until exclusive.
type HistoryParams struct {
	SinceMs int64 `json:"since_ms"`
	UntilMs int64 `json:"until_ms"`
}

func (d Deps) usage(ctx context.Context, _ rpc.Caller, _ rpc.None) (shippingcore.Report, error) {
	return d.Usage.Report(ctx)
}

func (d Deps) history(ctx context.Context, _ rpc.Caller, p HistoryParams) (shippingcore.History, error) {
	h, err := d.Usage.History(ctx, p.SinceMs, p.UntilMs)
	for i, b := range h.Buckets {
		h.Buckets[i].ModelLabel = d.ModelLabel(agent.Kind(b.Agent), b.Model)
	}
	return h, err
}
