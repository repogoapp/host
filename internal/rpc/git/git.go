// Package git is the `git.*` methods: branch, ahead/behind, what changed, and
// the mutations a phone may make to a checkout.
package git

import (
	"context"

	gitcore "github.com/repogo/host/internal/git"
	"github.com/repogo/host/internal/rpc"
)

type Deps struct {
	Git *gitcore.Service
}

func Register(r *rpc.Router, d Deps) {
	// status reports on several projects at once — the sidebar's call.
	rpc.Add(r, "git.status", d.status)
	rpc.Add(r, "git.changes", d.changes)
	// branches lists what a chat could fork from.
	rpc.Add(r, "git.branches", d.branches)
	// diff is the working tree's change set.
	rpc.Add(r, "git.diff", d.diff)
	// patch expands one change set: its files, or one file's hunks.
	rpc.Add(r, "git.patch", d.patch)

	// Mutations finish once started: a checkout left mid-pull is worse than
	// one whose result nobody saw.
	rpc.Add(r, "git.pull", d.pull, rpc.Detached)
	rpc.Add(r, "git.reset_hard", d.resetHard, rpc.Detached)
	rpc.Add(r, "git.switch_branch", d.switchBranch, rpc.Detached)
	rpc.Add(r, "git.create_branch", d.createBranch, rpc.Detached)
	rpc.Add(r, "git.commit_push", d.commitPush, rpc.Detached)
}

func (d Deps) status(ctx context.Context, _ rpc.Caller, a StatusParams) (StatusResult, error) {
	statuses, err := d.Git.Status(ctx, a.Paths)
	return StatusResult{Projects: statuses}, err
}

func (d Deps) changes(ctx context.Context, _ rpc.Caller, a PathParams) (ChangesResult, error) {
	changes, err := d.Git.Changes(ctx, a.Path)
	return ChangesResult{Path: a.Path, Files: changes}, err
}

func (d Deps) branches(ctx context.Context, _ rpc.Caller, a PathParams) (BranchesResult, error) {
	branches, err := d.Git.Branches(ctx, a.Path)
	return BranchesResult{Path: a.Path, Branches: branches}, err
}

func (d Deps) diff(ctx context.Context, _ rpc.Caller, a PathParams) (gitcore.History, error) {
	return d.Git.Diff(ctx, a.Path)
}

func (d Deps) patch(ctx context.Context, _ rpc.Caller, a PatchParams) (gitcore.Patch, error) {
	return d.Git.Patch(ctx, a.Path, a.Base, a.Head, a.File)
}

func (d Deps) pull(ctx context.Context, _ rpc.Caller, a PathParams) (rpc.Ack, error) {
	return rpc.OK, d.Git.Pull(ctx, a.Path)
}

func (d Deps) resetHard(ctx context.Context, _ rpc.Caller, a BranchParams) (rpc.Ack, error) {
	return rpc.OK, d.Git.ResetHard(ctx, a.Path, a.Branch)
}

func (d Deps) switchBranch(ctx context.Context, _ rpc.Caller, a BranchParams) (rpc.Ack, error) {
	return rpc.OK, d.Git.SwitchBranch(ctx, a.Path, a.Branch)
}

func (d Deps) createBranch(ctx context.Context, _ rpc.Caller, a CreateBranchParams) (rpc.Ack, error) {
	return rpc.OK, d.Git.CreateBranch(ctx, a.Path, a.Name, a.FromRef)
}

func (d Deps) commitPush(ctx context.Context, _ rpc.Caller, a CommitPushParams) (gitcore.CommitPushResult, error) {
	return d.Git.CommitPush(ctx, a.Path, a.Message)
}
