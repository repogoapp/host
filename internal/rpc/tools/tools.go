// Package tools is the `tools.*` methods: install, update, sign out
// and uninstall any CLI the host knows (agents, gh, cloud CLIs) by its kind,
// and for a CLI with several accounts, list them and pick one per project.
package tools

import (
	"context"
	"errors"

	"github.com/repogoapp/host/internal/clitool"
	"github.com/repogoapp/host/internal/mcp"
	"github.com/repogoapp/host/internal/rpc"
)

type Deps struct {
	Tools *clitool.Inventory
}

// KindParams names one tool; tools.update with an empty kind updates every
// tool that is behind.
type KindParams struct {
	Kind string `json:"kind"`
}

// AccountsParams name a CLI with several accounts and the project they're for.
type AccountsParams struct {
	Kind    string `json:"kind"`
	Project string `json:"project"`
}

// SelectAccountParams' id is the account the project uses; "" is the CLI's
// own default.
type SelectAccountParams struct {
	AccountsParams
	ID string `json:"id"`
}

// AccountParams name one of a CLI's accounts.
type AccountParams struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// RenameAccountParams' name is the account's new one.
type RenameAccountParams struct {
	AccountParams
	Name string `json:"name"`
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
	// The CLI's own sign-out; the phone hears the result as host.setup_changed too.
	rpc.Add(r, "tools.logout", d.logout, rpc.Detached)
	// A CLI's several accounts (AWS profiles), and which one a project's
	// agents use; signing one in goes through connect.start.
	rpc.Add(r, "tools.accounts", d.accounts)
	rpc.Add(r, "tools.select_account", d.selectAccount)
	// Rename moves every project's pick with it; remove signs the account
	// out and deletes it from the CLI's config, stored keys too.
	rpc.Add(r, "tools.rename_account", d.renameAccount)
	rpc.Add(r, "tools.remove_account", d.removeAccount, rpc.Detached)
}

func (d Deps) renameAccount(ctx context.Context, _ rpc.Caller, p RenameAccountParams) (rpc.Ack, error) {
	return rpc.OK, d.Tools.RenameAccount(ctx, p.Kind, p.ID, p.Name)
}

func (d Deps) removeAccount(ctx context.Context, _ rpc.Caller, p AccountParams) (rpc.Ack, error) {
	return rpc.OK, d.Tools.RemoveAccount(ctx, p.Kind, p.ID)
}

// accounts answers a CLI with one sign-in as unsupported rather than an
// error, so a Connect asks once and opens its sign-in instead.
func (d Deps) accounts(ctx context.Context, _ rpc.Caller, p AccountsParams) (clitool.AccountListing, error) {
	listing, err := d.Tools.Accounts(ctx, p.Kind, mcp.Root(p.Project))
	if errors.Is(err, clitool.ErrNoAccounts) {
		return clitool.AccountListing{Accounts: []clitool.CLIAccount{}}, nil
	}
	return listing, err
}

func (d Deps) selectAccount(ctx context.Context, _ rpc.Caller, p SelectAccountParams) (rpc.Ack, error) {
	return rpc.OK, d.Tools.SelectAccount(ctx, p.Kind, mcp.Root(p.Project), p.ID)
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

func (d Deps) logout(ctx context.Context, _ rpc.Caller, p KindParams) (clitool.UpdateResult, error) {
	return d.Tools.Logout(ctx, p.Kind)
}
