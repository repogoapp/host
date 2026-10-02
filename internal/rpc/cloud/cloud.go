// Package cloud exposes the cloud projects under a folder, and each one's
// environment variables, through host services.
package cloud

import (
	"context"

	"github.com/repogo/host/internal/cloudenv"
	"github.com/repogo/host/internal/cloudprojects"
	"github.com/repogo/host/internal/rpc"
)

type Deps struct {
	Projects *cloudprojects.Service
	Env      *cloudenv.Service
}

type ProjectsListParams struct {
	Cwd string `json:"cwd"`
}

// EnvListParams names a project as its card does.
type EnvListParams struct {
	Provider  string `json:"provider"`
	TeamID    string `json:"team_id"`
	ProjectID string `json:"project_id"`
}

// EnvValueParams names one variable by the id cloud.env.list gave it.
type EnvValueParams struct {
	Provider  string `json:"provider"`
	TeamID    string `json:"team_id"`
	ProjectID string `json:"project_id"`
	ID        string `json:"id"`
}

// EnvSetParams creates a variable when id is empty, else replaces that one;
// deploy puts the change live now instead of at the next deploy.
type EnvSetParams struct {
	Provider  string   `json:"provider"`
	TeamID    string   `json:"team_id"`
	ProjectID string   `json:"project_id"`
	ID        string   `json:"id"`
	Key       string   `json:"key"`
	Value     string   `json:"value"`
	Targets   []string `json:"targets"`
	GitBranch string   `json:"git_branch"`
	Secret    bool     `json:"secret"`
	Deploy    bool     `json:"deploy"`
}

type EnvRemoveParams struct {
	Provider  string `json:"provider"`
	TeamID    string `json:"team_id"`
	ProjectID string `json:"project_id"`
	ID        string `json:"id"`
	Deploy    bool   `json:"deploy"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "cloud.projects.list", d.projectsList)
	rpc.Add(r, "cloud.env.list", d.envList)
	rpc.Add(r, "cloud.env.value", d.envValue)
	rpc.Add(r, "cloud.env.set", d.envSet, rpc.Detached)
	rpc.Add(r, "cloud.env.remove", d.envRemove, rpc.Detached)
}

func (d Deps) projectsList(ctx context.Context, _ rpc.Caller, p ProjectsListParams) (cloudprojects.Result, error) {
	return d.Projects.List(ctx, p.Cwd)
}

func (d Deps) envList(ctx context.Context, _ rpc.Caller, p EnvListParams) (cloudenv.List, error) {
	return d.Env.List(ctx, cloudenv.Ref{Provider: p.Provider, TeamID: p.TeamID, ProjectID: p.ProjectID})
}

func (d Deps) envValue(ctx context.Context, _ rpc.Caller, p EnvValueParams) (cloudenv.Value, error) {
	return d.Env.Value(ctx, cloudenv.Ref{Provider: p.Provider, TeamID: p.TeamID, ProjectID: p.ProjectID}, p.ID)
}

func (d Deps) envSet(ctx context.Context, _ rpc.Caller, p EnvSetParams) (cloudenv.Var, error) {
	return d.Env.Set(ctx, cloudenv.Ref{Provider: p.Provider, TeamID: p.TeamID, ProjectID: p.ProjectID}, cloudenv.Write{
		ID: p.ID, Key: p.Key, Value: p.Value, Targets: p.Targets, GitBranch: p.GitBranch, Secret: p.Secret, Deploy: p.Deploy,
	})
}

func (d Deps) envRemove(ctx context.Context, _ rpc.Caller, p EnvRemoveParams) (rpc.Ack, error) {
	err := d.Env.Remove(ctx, cloudenv.Ref{Provider: p.Provider, TeamID: p.TeamID, ProjectID: p.ProjectID}, p.ID, p.Deploy)
	if err != nil {
		return rpc.Ack{}, err
	}
	return rpc.OK, nil
}
