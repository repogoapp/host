// Package agents is the `agents.*` methods: every agent a chat in a project
// can start with, for the agent picker. The environment's run through
// internal/agents; a project's own through internal/agents/acp.
package agents

import (
	"context"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agentcatalog"
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
	// Catalog is what each environment agent's CLI last said it offers.
	Catalog interface {
		Last(kind agent.Kind) (agentcatalog.Catalog, bool)
	}
}

type ListParams struct {
	// Project is the project's folder on this host.
	Project string `json:"project" doc:"The project's folder"`
}

type ListResult struct {
	Agents []AgentChoice `json:"agents" wire:"array"`
}

// AgentChoice is one agent the picker offers, as chats.start takes it: Model
// is the default of Models; Modes and PermissionModes are the values its
// config's mode and permission_mode accept. Icon is an https image or a logo name.
type AgentChoice struct {
	Agent           string       `json:"agent"`
	ProjectAgent    string       `json:"project_agent,omitempty"`
	Name            string       `json:"name"`
	Icon            string       `json:"icon,omitempty"`
	Error           string       `json:"error,omitempty"`
	Models          []AgentModel `json:"models,omitempty"`
	Model           string       `json:"model,omitempty"`
	Modes           []string     `json:"modes,omitempty"`
	PermissionModes []string     `json:"permission_modes,omitempty"`
}

// AgentModel is one model: ID as chats.start's config takes it, and the
// efforts its reasoning_level accepts, DefaultEffort when none is set.
// Icon is as AgentChoice's.
type AgentModel struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Icon          string   `json:"icon,omitempty"`
	Efforts       []string `json:"efforts,omitempty"`
	DefaultEffort string   `json:"default_effort,omitempty"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "agents.list", d.list, rpc.Tool("The agents a chat in a project can start with, "+
		"for chats.start: each one's models, default model, the efforts (reasoning_level) each "+
		"model accepts, and its modes and permission modes."), rpc.Read)
}

// list is the environment's agents, in the host's order, then the
// project's own by folder name. An environment agent's options are its cached
// catalog, so listing never waits on a CLI; one not read yet has none.
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
		choice := AgentChoice{Agent: string(a.Kind()), Name: a.Tool.Name}
		if catalog, ok := d.Catalog.Last(a.Kind()); ok {
			choice.Models = catalogModels(catalog.Models)
			choice.Model = catalog.DefaultModel
			choice.Modes, choice.PermissionModes = turnModes, turnPermissions
		}
		out = append(out, choice)
	}
	for _, a := range project {
		out = append(out, AgentChoice{
			Agent: string(agent.KindProject), ProjectAgent: a.ID, Name: a.Name, Icon: a.Icon,
			Error: a.Error, Models: projectModels(a.Models), Model: a.Model,
		})
	}
	return ListResult{Agents: out}, nil
}

// What chats.start's config takes for an environment agent: RepoGo's names,
// which each adapter maps onto its CLI's own, not the CLI's.
var (
	turnModes       = []string{"agent", "plan"}
	turnPermissions = []string{
		string(agent.PermissionApprovalRequired), string(agent.PermissionAutoAcceptEdits), string(agent.PermissionFullAccess),
	}
)

// catalogModels is the models the CLI's own picker offers, hidden ones left out.
func catalogModels(models []agentcatalog.Model) []AgentModel {
	var out []AgentModel
	for _, m := range models {
		if m.Hidden {
			continue
		}
		out = append(out, AgentModel{ID: m.ID, Name: m.Name, Efforts: m.Efforts, DefaultEffort: m.DefaultEffort})
	}
	return out
}

func projectModels(models []projectagent.ProjectAgentModel) []AgentModel {
	var out []AgentModel
	for _, m := range models {
		out = append(out, AgentModel{ID: m.ID, Name: m.Name, Icon: m.Icon})
	}
	return out
}
