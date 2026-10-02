// Package github is the `github.*` methods: how a project gets onto a host,
// and how what came out of it ships.
package github

import (
	"context"

	ghcore "github.com/repogo/host/internal/github"
	"github.com/repogo/host/internal/rpc"
)

type Deps struct {
	GitHub *ghcore.Service
}

func Register(r *rpc.Router, d Deps) {
	// avatar is the signed-in account's picture, so a client can show whose
	// GitHub is behind a host rather than only which machine it is.
	rpc.Add(r, "github.avatar", d.avatar)
	rpc.Add(r, "github.repos", d.repos)
	// Remote-reachable: cloning picks a folder under one fixed directory.
	// Synchronous: the reply is the completion. A job would need a progress lane.
	rpc.Add(r, "github.clone", d.clone, rpc.Detached)

	// Remote: a paired device may open a pull request for what is already
	// pushed in a project it can reach. It names a path, not a remote.
	rpc.Add(r, "github.pr_create", d.prCreate, rpc.Detached)
	// publish gives a folder with no GitHub repository one, under the host's
	// sign-in; the client then commits and pushes with git.commit_push.
	rpc.Add(r, "github.publish", d.publish, rpc.Detached)
}

func (d Deps) avatar(ctx context.Context, _ rpc.Caller, a AvatarParams) (ghcore.Avatar, error) {
	return d.GitHub.Avatar(ctx, a.IfNoneMatch)
}

func (d Deps) repos(ctx context.Context, _ rpc.Caller, a ReposParams) (ReposResult, error) {
	repos, err := d.GitHub.Repos(ctx, a.Owner, a.Query, a.Limit)
	return ReposResult{Repos: repos}, err
}

func (d Deps) clone(ctx context.Context, _ rpc.Caller, a CloneParams) (ghcore.Clone, error) {
	return d.GitHub.Clone(ctx, a.NameWithOwner)
}

func (d Deps) prCreate(ctx context.Context, _ rpc.Caller, a PRCreateParams) (ghcore.PullRequest, error) {
	return d.GitHub.CreatePR(ctx, a.Path, ghcore.NewPR{Title: a.Title, Body: a.Body, Base: a.Base, Draft: a.Draft})
}

func (d Deps) publish(ctx context.Context, _ rpc.Caller, a PublishParams) (ghcore.Published, error) {
	return d.GitHub.Publish(ctx, a.Path, a.Name, a.Private)
}
