// Package agents is the `agents.*` methods: every agent a chat in a project
// can start with, for the agent picker. The environment's run through
// internal/agents; a project's own through internal/agents/acp.
package agents

import (
	"context"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/files"
	"github.com/repogoapp/host/internal/projectagent"
	"github.com/repogoapp/host/internal/rpc"
)

type Deps struct {
	Files *files.Service
	// Environment is the agents this host installs and signs in.
	Environment agent.Providers
	// Project lists each project's own agents.
	Project projectagent.Lister
}

type ListParams struct {
	// Project is the project's folder on this host.
	Project string `json:"project"`
}

type ListResult struct {
	Agents []AgentChoice `json:"agents" wire:"array"`
}

// AgentChoice is one agent the picker offers, as chats.start takes it. Models
// (Model the default) are a project agent's; an environment agent's come with
// host.setup, with their options.
type AgentChoice struct {
	Agent        string                           `json:"agent"`
	ProjectAgent string                           `json:"project_agent,omitempty"`
	Name         string                           `json:"name"`
	Error        string                           `json:"error,omitempty"`
	Models       []projectagent.ProjectAgentModel `json:"models,omitempty"`
	Model        string                           `json:"model,omitempty"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "agents.list", d.list)
}

// list is the environment's agents, in the host's order, then the
// project's own by folder name.
func (d Deps) list(_ context.Context, _ rpc.Caller, p ListParams) (ListResult, error) {
	path, err := d.Files.Contain(p.Project)
	if err != nil {
		return ListResult{}, err
	}
	project, err := d.Project.ProjectAgents(path)
	if err != nil {
		return ListResult{}, err
	}
	out := make([]AgentChoice, 0, len(d.Environment)+len(project))
	for _, a := range d.Environment {
		out = append(out, AgentChoice{Agent: string(a.Kind()), Name: a.Tool.Name})
	}
	for _, a := range project {
		out = append(out, AgentChoice{
			Agent: string(agent.KindProject), ProjectAgent: a.ID, Name: a.Name,
			Error: a.Error, Models: a.Models, Model: a.Model,
		})
	}
	return ListResult{Agents: out}, nil
}
