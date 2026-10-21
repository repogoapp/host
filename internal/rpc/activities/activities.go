// Package activities is the `activities.*` methods: the step Live Activities
// plugins show on the phones; the pushing lives in internal/stepactivity.
package activities

import (
	"context"
	"fmt"

	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/rpc"
	"github.com/repogoapp/host/internal/stepactivity"
)

type Deps struct {
	Activities *stepactivity.Driver
}

func Register(r *rpc.Router, d Deps) {
	// Plugins: ctx.activity reaches it through ctx.host.
	rpc.Add(r, "activities.set", d.set, rpc.Plugins)
}

// set keys the activity by the calling plugin, which no param can name.
func (d Deps) set(_ context.Context, c rpc.Caller, a stepactivity.Request) (rpc.Ack, error) {
	if c.Plugin == "" {
		return rpc.Ack{}, fmt.Errorf("%w: activities.set is for plugins", errkind.ErrInvalid)
	}
	return rpc.OK, d.Activities.Set(c.Plugin, a)
}
