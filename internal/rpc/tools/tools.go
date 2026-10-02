// Package tools is the `tools.*` methods: install, update, sign in, sign out
// and uninstall any CLI the host knows (agents, gh, cloud CLIs) by its kind.
package tools

import (
	"context"

	"github.com/repogo/host/internal/clilogin"
	"github.com/repogo/host/internal/clitool"
	"github.com/repogo/host/internal/rpc"
)

type Deps struct {
	Tools *clitool.Inventory
}

// KindParams names one tool; tools.update with an empty kind updates every
// tool that is behind.
type KindParams struct {
	Kind string `json:"kind"`
}

// LoginCompleteParams' code is what the tool's sign-in page showed.
type LoginCompleteParams struct {
	Kind string `json:"kind"`
	Code string `json:"code"`
}

// LoginsResult names the tools with a sign-in still waiting on the user.
type LoginsResult struct {
	Waiting []string `json:"waiting" wire:"array"`
}

type UpdateResponse struct {
	Updated []clitool.UpdateResult `json:"updated" wire:"array"`
}

func Register(r *rpc.Router, d Deps) {
	// Remote-callable: no caller-supplied path, what runs is decided by how
	// the binary was installed, and the caller only names which tool.
	rpc.Add(r, "tools.install", d.install, rpc.Detached)
	rpc.Add(r, "tools.update", d.update, rpc.Detached)
	rpc.Add(r, "tools.uninstall", d.uninstall, rpc.Detached)
	// The CLI signs in on the host; the phone opens its page and, for Claude,
	// sends back the code the page shows. The end arrives as host.setup_changed.
	rpc.Add(r, "tools.login_start", d.loginStart)
	rpc.Add(r, "tools.login_complete", d.loginComplete, rpc.Detached)
	rpc.Add(r, "tools.login_cancel", d.loginCancel)
	// The CLI's own sign-out; the phone hears the result as host.setup_changed too.
	rpc.Add(r, "tools.logout", d.logout, rpc.Detached)
	// logins is which sign-ins still wait; cheap, for a sheet to poll.
	rpc.Add(r, "tools.logins", d.logins)
}

func (d Deps) install(ctx context.Context, _ rpc.Caller, p KindParams) (clitool.UpdateResult, error) {
	return d.Tools.Install(ctx, p.Kind)
}

func (d Deps) update(ctx context.Context, _ rpc.Caller, p KindParams) (UpdateResponse, error) {
	if p.Kind == "" {
		return UpdateResponse{Updated: d.Tools.UpdateAll(ctx)}, nil
	}
	return UpdateResponse{Updated: []clitool.UpdateResult{d.Tools.Update(ctx, p.Kind)}}, nil
}

func (d Deps) uninstall(ctx context.Context, _ rpc.Caller, p KindParams) (clitool.UpdateResult, error) {
	return d.Tools.Uninstall(ctx, p.Kind)
}

func (d Deps) loginStart(ctx context.Context, _ rpc.Caller, p KindParams) (clilogin.Code, error) {
	return d.Tools.LoginStart(ctx, p.Kind)
}

func (d Deps) loginComplete(ctx context.Context, _ rpc.Caller, p LoginCompleteParams) (rpc.Ack, error) {
	if err := d.Tools.LoginComplete(ctx, p.Kind, p.Code); err != nil {
		return rpc.Ack{}, err
	}
	return rpc.OK, nil
}

func (d Deps) loginCancel(_ context.Context, _ rpc.Caller, p KindParams) (rpc.Ack, error) {
	d.Tools.LoginCancel(p.Kind)
	return rpc.OK, nil
}

func (d Deps) logout(ctx context.Context, _ rpc.Caller, p KindParams) (clitool.UpdateResult, error) {
	return d.Tools.Logout(ctx, p.Kind)
}

func (d Deps) logins(context.Context, rpc.Caller, rpc.None) (LoginsResult, error) {
	return LoginsResult{Waiting: d.Tools.Logins()}, nil
}
