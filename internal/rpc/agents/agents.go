// Package agents is the `agents.*` methods: the agents a project brings in
// `.repogo/agents`, for the agent picker; internal/agents/acp runs them.
package agents

import (
	"context"

	"github.com/repogoapp/host/internal/files"
	"github.com/repogoapp/host/internal/projectagent"
	"github.com/repogoapp/host/internal/rpc"
)

type Deps struct {
	Files  *files.Service
	Agents projectagent.Lister
}

type ListParams struct {
	// Project is the project's folder on this host.
	Project string `json:"project"`
}

type ListResult struct {
	Agents []projectagent.ProjectAgent `json:"agents" wire:"array"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "agents.list", d.list)
}

// list is the project's own agents; a chat starts one with chats.start's
// agent acp and project_agent its id.
func (d Deps) list(_ context.Context, _ rpc.Caller, p ListParams) (ListResult, error) {
	path, err := d.Files.Contain(p.Project)
	if err != nil {
		return ListResult{}, err
	}
	agents, err := d.Agents.ProjectAgents(path)
	return ListResult{Agents: agents}, err
}
