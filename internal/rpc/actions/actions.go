// Package actions is the `actions.*` methods: the caller names an action off
// the project's own actions.json, never a command.
package actions

import (
	"context"

	actionscore "github.com/repogo/host/internal/actions"
	"github.com/repogo/host/internal/rpc"
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

type ListResult struct {
	Actions []actionscore.Action `json:"actions" wire:"array"`
}

type StartResult struct {
	RunID string `json:"run_id"`
}

type StopParams struct {
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
}

func (d Deps) list(_ context.Context, _ rpc.Caller, a ListParams) (ListResult, error) {
	actions, err := d.Actions.List(a.Path)
	return ListResult{Actions: actions}, err
}

func (d Deps) run(ctx context.Context, _ rpc.Caller, a ActionParams) (actionscore.Result, error) {
	return d.Actions.Run(ctx, a.Path, a.Name)
}

func (d Deps) start(_ context.Context, _ rpc.Caller, a ActionParams) (StartResult, error) {
	id, err := d.Actions.Start(a.Path, a.Name)
	return StartResult{RunID: id}, err
}

func (d Deps) stop(_ context.Context, _ rpc.Caller, a StopParams) (rpc.Ack, error) {
	return rpc.OK, d.Actions.Stop(a.RunID)
}
