// Package shipping is the `usage.history` method: what this machine's agents
// spent, for the phone's usage screen. Only counts and costs, never content.
package shipping

import (
	"context"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/device"
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
	// Device names the caller for the screen time it records, from the pairing.
	Device func(device.ID) shippingcore.Device
}

func Register(r *rpc.Router, d Deps) {
	// Remote: the phone adds every environment's answer into one screen.
	rpc.Add(r, "usage.history", d.history)
	rpc.Add(r, "usage.time", d.time)
	// Screen time is a paired device's own, so the machine's CLI can't record it.
	rpc.Add(r, "usage.record_time", d.recordTime, rpc.Paired)
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

// TimeParams is a range of Unix milliseconds, until exclusive, scoped as
// HistoryParams is.
type TimeParams struct {
	SinceMs    int64  `json:"since_ms"`
	UntilMs    int64  `json:"until_ms"`
	Cwd        string `json:"cwd,omitempty"`
	RootCommit string `json:"root_commit,omitempty"`
}

// Time is every span of activity in the range: devices' screen time and agents'
// work, never totals. A device counts the union across environments itself.
type Time struct {
	Spans []shippingcore.Span `json:"spans" wire:"array"`
}

func (d Deps) time(ctx context.Context, _ rpc.Caller, p TimeParams) (Time, error) {
	scope, err := d.scope(HistoryParams{Cwd: p.Cwd, RootCommit: p.RootCommit})
	if err != nil {
		return Time{}, err
	}
	spans, err := d.Usage.Activity(ctx, p.SinceMs, p.UntilMs, scope, time.Now())
	return Time{Spans: spans}, err
}

// RecordTimeParams is a device's screen time and prompting, each span under its own ID.
type RecordTimeParams struct {
	Spans []shippingcore.UserSpan `json:"spans" wire:"array"`
}

// Recorded says how many spans were skipped as impossible; the rest are stored.
type Recorded struct {
	Skipped int `json:"skipped"`
}

func (d Deps) recordTime(ctx context.Context, c rpc.Caller, p RecordTimeParams) (Recorded, error) {
	skipped, err := d.Usage.RecordUser(ctx, d.Device(c.Device), p.Spans, time.Now())
	return Recorded{Skipped: skipped}, err
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
