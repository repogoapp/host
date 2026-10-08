// Package actions is the `actions.*` methods: the caller names an action off
// the project's own actions.json, never a command.
package actions

import (
	"context"

	actionscore "github.com/repogo/host/internal/actions"
	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/store"
)

type Deps struct {
	Actions *actionscore.Service
}

type ActionParams struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

type ListParams struct {
	Path string `json:"path"`
}

type StartResult struct {
	Run store.ActionRun `json:"run"`
}

type RunParams struct {
	RunID string `json:"run_id"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "actions.list", d.list)
	// run blocks until the command finishes and answers with its output. The
	// client sets its call timeout from the action's own timeoutMs, so a slow
	// action is the caller's informed choice; it runs to the end regardless.
	rpc.Add(r, "actions.run", d.run, rpc.Detached)
	rpc.Add(r, "actions.start", d.start)
	rpc.Add(r, "actions.stop", d.stop)
	rpc.Add(r, "actions.output", d.output)
}

func (d Deps) list(_ context.Context, _ rpc.Caller, a ListParams) (actionscore.Snapshot, error) {
	return d.Actions.List(a.Path)
}

func (d Deps) run(ctx context.Context, c rpc.Caller, a ActionParams) (actionscore.Result, error) {
	return d.Actions.Run(ctx, c.Device, a.Path, a.Name)
}

func (d Deps) start(_ context.Context, c rpc.Caller, a ActionParams) (StartResult, error) {
	run, err := d.Actions.Start(c.Device, a.Path, a.Name)
	return StartResult{Run: run}, err
}

func (d Deps) stop(_ context.Context, _ rpc.Caller, a RunParams) (rpc.Ack, error) {
	return rpc.OK, d.Actions.Stop(a.RunID)
}

func (d Deps) output(_ context.Context, _ rpc.Caller, a RunParams) (actionscore.Output, error) {
	return d.Actions.Output(a.RunID)
}
