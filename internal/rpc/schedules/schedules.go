// Package schedules is the `schedules.*` methods: the prompts this host runs
// on its own at a set time; the running lives in internal/schedule.
package schedules

import (
	"context"

	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/schedule"
	"github.com/repogo/host/internal/store"
)

type Deps struct {
	Schedules *schedule.Scheduler
}

type DeleteParams struct {
	ID string `json:"id"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "schedules.list", d.list)
	rpc.Add(r, "schedules.save", d.save)
	rpc.Add(r, "schedules.delete", d.delete)
}

func (d Deps) list(context.Context, rpc.Caller, rpc.None) (schedule.Changed, error) {
	return d.Schedules.List()
}

func (d Deps) save(_ context.Context, _ rpc.Caller, a store.Schedule) (schedule.Row, error) {
	return d.Schedules.Save(a)
}

func (d Deps) delete(_ context.Context, _ rpc.Caller, a DeleteParams) (rpc.Ack, error) {
	return rpc.OK, d.Schedules.Delete(a.ID)
}
