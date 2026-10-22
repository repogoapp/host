// Package connect is the `connect.*` methods: the phone connects a key, an MCP
// server or a CLI's sign-in on this environment, and cards and agents read
// whether it is connected, never with what. The flows live in internal/connect.
package connect

import (
	"context"

	connectcore "github.com/repogoapp/host/internal/connect"
	"github.com/repogoapp/host/internal/rpc"
)

type Deps struct {
	Connect *connectcore.Service
}

type ListParams struct {
	Project string             `json:"project" doc:"The project the card or agent is in"`
	Targets []connectcore.Want `json:"targets" doc:"The targets to report on"`
}

type ListResult struct {
	Connections []connectcore.Connection `json:"connections" wire:"array"`
}

type FlowParams struct {
	FlowID string `json:"flow_id" doc:"The flow's id, as connect.start gave it"`
}

type FlowResult struct {
	Flow connectcore.Flow `json:"flow"`
}

type SubmitParams struct {
	FlowID string `json:"flow_id"`
	StepID string `json:"step_id"`
	Value  string `json:"value"`
}

// CallbackParams is what an OAuth redirect brought back to a flow.
type CallbackParams struct {
	FlowID string `json:"flow_id"`
	State  string `json:"state"`
	Code   string `json:"code,omitempty"`
	// Error is the provider's own, when the user declined or it failed.
	Error string `json:"error,omitempty"`
}

// Only the reads are tools: an agent or a card learns whether something is
// connected, and only the phone's own sheet starts a flow or sends a value.
func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "connect.list", d.list, rpc.Tool("Whether each target is connected on this environment: an env key "+
		"(env:NAME with its scope), an MCP server (mcp:<url>) or a CLI's sign-in (cli:<tool>); never with what."), rpc.Read, rpc.LiveOn("connect.changed"))
	rpc.Add(r, "connect.get", d.get, rpc.Tool("A connect flow the user was shown: waiting, or how it ended."),
		rpc.Read, rpc.LiveOn("connect.changed"))
	rpc.Add(r, "connect.start", d.start)
	rpc.Add(r, "connect.submit", d.submit)
	// Detached: the code is spent once the token endpoint has it.
	rpc.Add(r, "connect.callback", d.callback, rpc.Detached)
	rpc.Add(r, "connect.cancel", d.cancel)
}

func (d Deps) list(ctx context.Context, _ rpc.Caller, a ListParams) (ListResult, error) {
	return ListResult{Connections: d.Connect.List(ctx, a.Project, a.Targets)}, nil
}

func (d Deps) get(_ context.Context, _ rpc.Caller, a FlowParams) (FlowResult, error) {
	f, err := d.Connect.Get(a.FlowID)
	return FlowResult{Flow: f}, err
}

func (d Deps) start(ctx context.Context, _ rpc.Caller, a connectcore.StartParams) (FlowResult, error) {
	f, err := d.Connect.Start(ctx, a)
	return FlowResult{Flow: f}, err
}

func (d Deps) submit(ctx context.Context, _ rpc.Caller, a SubmitParams) (FlowResult, error) {
	f, err := d.Connect.Submit(ctx, a.FlowID, a.StepID, a.Value)
	return FlowResult{Flow: f}, err
}

func (d Deps) callback(ctx context.Context, _ rpc.Caller, a CallbackParams) (FlowResult, error) {
	f, err := d.Connect.Callback(ctx, a.FlowID, a.State, a.Code, a.Error)
	return FlowResult{Flow: f}, err
}

func (d Deps) cancel(_ context.Context, _ rpc.Caller, a FlowParams) (rpc.Ack, error) {
	return rpc.OK, d.Connect.Cancel(a.FlowID)
}
