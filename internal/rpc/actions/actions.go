// Package actions is the `actions.*` methods: the caller names an action off
// the project's own actions.json, never a command.
package actions

import (
	"context"

	actionscore "github.com/repogoapp/host/internal/actions"
	"github.com/repogoapp/host/internal/rpc"
)

type Deps struct {
	Actions *actionscore.Service
}

type ActionParams struct {
	Path string `json:"path" doc:"The project's folder"`
	Name string `json:"name" doc:"The action's name, as actions.list gives it"`
	// Secrets is the variables of the action's envFrom handles that this
	// host does not bind, read by the phone, keyed by handle.
	Secrets map[string]map[string]string `json:"secrets,omitempty" doc:"Leave empty"`
}

type ListParams struct {
	Path string `json:"path" doc:"The project's folder"`
}

type StartResult struct {
	Run actionscore.Run `json:"run"`
}

type RunParams struct {
	RunID string `json:"run_id" doc:"The run's id, as actions.start or actions.list gives it"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "actions.list", d.list, rpc.Tool("A project's actions (the commands its actions.json declares) and "+
		"their runs."), rpc.Read, rpc.LiveOn("actions.changed"))
	// run blocks until the command finishes and answers with its output. The
	// client sets its call timeout from the action's own timeoutMs, so a slow
	// action is the caller's informed choice; it runs to the end regardless.
	rpc.Add(r, "actions.run", d.run, rpc.Detached, rpc.Tool("Run an action to the end and answer "+
		"with its output."), rpc.Write)
	rpc.Add(r, "actions.start", d.start, rpc.Tool("Start an action that keeps running, such as a dev "+
		"server; answers with its run."), rpc.Write)
	rpc.Add(r, "actions.stop", d.stop, rpc.Tool("Stop an action's run."), rpc.Write)
	rpc.Add(r, "actions.output", d.output)
}

func (d Deps) list(_ context.Context, _ rpc.Caller, a ListParams) (actionscore.Snapshot, error) {
	return d.Actions.List(a.Path)
}

func (d Deps) run(ctx context.Context, c rpc.Caller, a ActionParams) (actionscore.Result, error) {
	return d.Actions.Run(ctx, c.Device, a.Path, a.Name, a.Secrets)
}

func (d Deps) start(_ context.Context, c rpc.Caller, a ActionParams) (StartResult, error) {
	run, err := d.Actions.Start(c.Device, a.Path, a.Name, a.Secrets)
	return StartResult{Run: run}, err
}

func (d Deps) stop(_ context.Context, _ rpc.Caller, a RunParams) (rpc.Ack, error) {
	return rpc.OK, d.Actions.Stop(a.RunID)
}

func (d Deps) output(_ context.Context, _ rpc.Caller, a RunParams) (actionscore.Output, error) {
	return d.Actions.Output(a.RunID)
}
