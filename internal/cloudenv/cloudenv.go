// Package cloudenv reads and writes a cloud project's environment variables
// through its provider's CLI, in one shape for Vercel, Fly and Cloudflare.
package cloudenv

import (
	"context"
	"fmt"
	"regexp"

	"github.com/repogo/host/internal/cloudflare"
	"github.com/repogo/host/internal/cloudscan"
	"github.com/repogo/host/internal/errkind"
	"github.com/repogo/host/internal/fly"
	"github.com/repogo/host/internal/vercel"
)

// Var is one environment variable. ID is Vercel's env id, and the key on Fly
// and Cloudflare, which name a variable by its key. Targets and GitBranch
// are Vercel's; the other two have one environment per project.
type Var struct {
	ID        string   `json:"id"`
	Key       string   `json:"key"`
	Secret    bool     `json:"secret"`
	Targets   []string `json:"targets" wire:"array"`
	GitBranch string   `json:"git_branch"`
}

// List carries the targets the provider offers; empty hides the picker.
type List struct {
	Vars    []Var    `json:"vars" wire:"array"`
	Targets []string `json:"targets" wire:"array"`
}

type Value struct {
	Value string `json:"value"`
}

// Ref is a project card's provider, team (Vercel team or Cloudflare account)
// and project id (Vercel project, Fly app or Worker name).
type Ref struct {
	Provider  string
	TeamID    string
	ProjectID string
}

// Write creates a variable when ID is empty, else replaces that one. Deploy
// puts it live now; without it, it waits for the project's next deploy.
type Write struct {
	ID        string
	Key       string
	Value     string
	Targets   []string
	GitBranch string
	Secret    bool
	Deploy    bool
}

// Providers are the CLI calls, one set per provider; New takes the real ones.
type Providers struct {
	VercelList     func(ctx context.Context, teamID, projectID string) ([]vercel.EnvVar, error)
	VercelValue    func(ctx context.Context, teamID, projectID, id string) (string, error)
	VercelSet      func(context.Context, vercel.EnvWrite) (vercel.EnvVar, error)
	VercelRemove   func(ctx context.Context, teamID, projectID, id string) error
	VercelRedeploy func(ctx context.Context, teamID, projectID string) error
	FlyList        func(ctx context.Context, app string) ([]fly.Secret, error)
	FlySet         func(ctx context.Context, app, name, value string, deploy bool) error
	FlyUnset       func(ctx context.Context, app, name string, deploy bool) error
	WorkerList     func(ctx context.Context, worker, accountID string) ([]cloudflare.Secret, error)
	WorkerPut      func(ctx context.Context, worker, accountID, name, value string, deploy bool) error
	WorkerDelete   func(ctx context.Context, worker, accountID, name string, deploy bool) error
}

type Service struct{ p Providers }

func New() *Service {
	return &Service{p: Providers{
		VercelList: vercel.ListEnv, VercelValue: vercel.EnvValue, VercelSet: vercel.SetEnv,
		VercelRemove: vercel.RemoveEnv, VercelRedeploy: vercel.RedeployProduction,
		FlyList: fly.ListSecrets, FlySet: fly.SetSecret, FlyUnset: fly.UnsetSecret,
		WorkerList: cloudflare.ListSecrets, WorkerPut: cloudflare.PutSecret, WorkerDelete: cloudflare.DeleteSecret,
	}}
}

// keyPattern is the shell's rule for a variable name, which all three accept.
var keyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var errProvider = fmt.Errorf("%w: provider is vercel, fly or cloudflare", errkind.ErrInvalid)

func (s *Service) List(ctx context.Context, r Ref) (List, error) {
	if r.ProjectID == "" {
		return List{}, fmt.Errorf("%w: project_id is required", errkind.ErrInvalid)
	}
	switch r.Provider {
	case cloudscan.ProviderVercel:
		rows, err := s.p.VercelList(ctx, r.TeamID, r.ProjectID)
		if err != nil {
			return List{}, err
		}
		list := List{Vars: make([]Var, 0, len(rows)), Targets: vercel.EnvTargets}
		for _, e := range rows {
			list.Vars = append(list.Vars, vercelVar(e))
		}
		return list, nil
	case cloudscan.ProviderFly:
		rows, err := s.p.FlyList(ctx, r.ProjectID)
		if err != nil {
			return List{}, err
		}
		list := List{Vars: make([]Var, 0, len(rows)), Targets: []string{}}
		for _, row := range rows {
			list.Vars = append(list.Vars, secretVar(row.Name))
		}
		return list, nil
	case cloudscan.ProviderCloudflare:
		rows, err := s.p.WorkerList(ctx, r.ProjectID, r.TeamID)
		if err != nil {
			return List{}, err
		}
		list := List{Vars: make([]Var, 0, len(rows)), Targets: []string{}}
		for _, row := range rows {
			list.Vars = append(list.Vars, secretVar(row.Name))
		}
		return list, nil
	}
	return List{}, errProvider
}

// Value reads one value back. Only Vercel keeps values that aren't secret.
func (s *Service) Value(ctx context.Context, r Ref, id string) (Value, error) {
	switch r.Provider {
	case cloudscan.ProviderVercel:
		value, err := s.p.VercelValue(ctx, r.TeamID, r.ProjectID, id)
		return Value{Value: value}, err
	case cloudscan.ProviderFly, cloudscan.ProviderCloudflare:
		return Value{}, fmt.Errorf("%w: a secret can't be read back", errkind.ErrInvalid)
	}
	return Value{}, errProvider
}

func (s *Service) Set(ctx context.Context, r Ref, w Write) (Var, error) {
	if r.ProjectID == "" {
		return Var{}, fmt.Errorf("%w: project_id is required", errkind.ErrInvalid)
	}
	if !keyPattern.MatchString(w.Key) {
		return Var{}, fmt.Errorf("%w: a key is letters, digits and underscores, not starting with a digit", errkind.ErrInvalid)
	}
	switch r.Provider {
	case cloudscan.ProviderVercel:
		e, err := s.p.VercelSet(ctx, vercel.EnvWrite{TeamID: r.TeamID, ProjectID: r.ProjectID, ID: w.ID, Key: w.Key,
			Value: w.Value, Targets: w.Targets, GitBranch: w.GitBranch, Sensitive: w.Secret})
		if err != nil {
			return Var{}, err
		}
		return vercelVar(e), s.redeploy(ctx, r, w.Deploy)
	case cloudscan.ProviderFly:
		if err := checkSecretWrite(w); err != nil {
			return Var{}, err
		}
		return secretVar(w.Key), s.p.FlySet(ctx, r.ProjectID, w.Key, w.Value, w.Deploy)
	case cloudscan.ProviderCloudflare:
		if err := checkSecretWrite(w); err != nil {
			return Var{}, err
		}
		return secretVar(w.Key), s.p.WorkerPut(ctx, r.ProjectID, r.TeamID, w.Key, w.Value, w.Deploy)
	}
	return Var{}, errProvider
}

func (s *Service) Remove(ctx context.Context, r Ref, id string, deploy bool) error {
	if r.ProjectID == "" || id == "" {
		return fmt.Errorf("%w: project_id and id are required", errkind.ErrInvalid)
	}
	switch r.Provider {
	case cloudscan.ProviderVercel:
		if err := s.p.VercelRemove(ctx, r.TeamID, r.ProjectID, id); err != nil {
			return err
		}
		return s.redeploy(ctx, r, deploy)
	case cloudscan.ProviderFly:
		return s.p.FlyUnset(ctx, r.ProjectID, id, deploy)
	case cloudscan.ProviderCloudflare:
		return s.p.WorkerDelete(ctx, r.ProjectID, r.TeamID, id, deploy)
	}
	return errProvider
}

// redeploy runs after the write has landed, so its failure says the change
// is saved and only the deploy is left.
func (s *Service) redeploy(ctx context.Context, r Ref, deploy bool) error {
	if !deploy {
		return nil
	}
	if err := s.p.VercelRedeploy(ctx, r.TeamID, r.ProjectID); err != nil {
		return fmt.Errorf("saved, but couldn't redeploy: %w", err)
	}
	return nil
}

// checkSecretWrite refuses what only Vercel has, rather than dropping it.
func checkSecretWrite(w Write) error {
	if len(w.Targets) > 0 || w.GitBranch != "" {
		return fmt.Errorf("%w: only Vercel variables have targets and branches", errkind.ErrInvalid)
	}
	if w.Value == "" {
		return fmt.Errorf("%w: value is required", errkind.ErrInvalid)
	}
	return nil
}

func vercelVar(e vercel.EnvVar) Var {
	return Var{ID: e.ID, Key: e.Key, Secret: e.Sensitive, Targets: e.Targets, GitBranch: e.GitBranch}
}

func secretVar(name string) Var {
	return Var{ID: name, Key: name, Secret: true, Targets: []string{}}
}
