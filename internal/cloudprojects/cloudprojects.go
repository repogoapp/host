// Package cloudprojects builds the Cloud sheet's cards: every project
// cloudscan finds under a folder whose provider's CLI is installed and signed
// in, read from its provider in parallel.
package cloudprojects

import (
	"cmp"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/repogo/host/internal/cloudflare"
	"github.com/repogo/host/internal/cloudscan"
	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/fly"
	"github.com/repogo/host/internal/par"
	"github.com/repogo/host/internal/vercel"
)

// Deployment keeps the latest deployment visible until another replaces it.
type Deployment struct {
	State  string    `json:"state"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}

// Project is one card. An empty ID means the folder isn't linked to a
// provider project yet; Error says why the provider couldn't be read. TeamID
// is the Vercel team or Cloudflare account that a follow-up read needs.
type Project struct {
	Provider   string              `json:"provider"`
	TeamID     string              `json:"team_id"`
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	URL        string              `json:"url"`
	Directory  string              `json:"directory"`
	Error      string              `json:"error"`
	Deployment *Deployment         `json:"deployment,omitempty"`
	Machines   *fly.MachineSummary `json:"machines,omitempty"`
}

type Result struct {
	Cwd      string            `json:"cwd"`
	Projects []Project         `json:"projects" wire:"array"`
	Issues   []cloudscan.Issue `json:"issues" wire:"array"`

	// NeedsSetup is each provider with projects here whose CLI is missing or
	// signed out: its projects aren't cards until it is ready.
	NeedsSetup []string `json:"needs_setup" wire:"array"`
}

type Service struct {
	scanner *cloudscan.Scanner
	ready   func(ctx context.Context, provider string) bool
	vercel  func(ctx context.Context, teamID, projectID string) (vercel.Project, error)
	fly     func(ctx context.Context, app string) (fly.App, error)
	worker  func(ctx context.Context, name, accountID string) (cloudflare.Worker, error)
}

// New takes ready, which says whether a provider's CLI is installed and
// signed in; the tool inventory answers it.
func New(paths files.Container, ready func(ctx context.Context, provider string) bool) *Service {
	return &Service{scanner: cloudscan.New(paths), ready: ready, vercel: vercel.Read, fly: fly.Read, worker: cloudflare.Read}
}

// List reads every project under path whose provider's CLI is ready; the
// others are named in NeedsSetup. A project that fails to read keeps its card
// with the error, so one bad link doesn't hide the others.
func (s *Service) List(ctx context.Context, path string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	found, err := s.scanner.Detect(ctx, path)
	if err != nil {
		return Result{}, err
	}

	// A project linked in two folders is one card, the first by path.
	seen := map[string]bool{}
	configs := []cloudscan.Project{}
	for _, p := range found.Projects {
		key := p.Provider + "\x00" + p.TeamID + "\x00" + p.ID
		if p.ID != "" && seen[key] {
			continue
		}
		seen[key] = true
		configs = append(configs, p)
	}

	configs, needsSetup := s.readyOnly(ctx, configs)
	projects := make([]Project, len(configs))
	var workers sync.WaitGroup
	slots := make(chan struct{}, 4)
	for i, config := range configs {
		workers.Add(1)
		go func() {
			defer workers.Done()
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()
			projects[i] = s.read(ctx, config)
		}()
	}
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{Cwd: found.Cwd, Projects: projects, Issues: found.Issues, NeedsSetup: needsSetup}, nil
}

// readyOnly keeps the projects whose provider's CLI is ready, asking each
// provider once and in parallel, and names the providers that aren't.
func (s *Service) readyOnly(ctx context.Context, configs []cloudscan.Project) ([]cloudscan.Project, []string) {
	providers := []string{}
	for _, p := range configs {
		if !slices.Contains(providers, p.Provider) {
			providers = append(providers, p.Provider)
		}
	}
	slices.Sort(providers)
	ready := par.Map(providers, 0, func(provider string) bool { return s.ready(ctx, provider) })

	kept := []cloudscan.Project{}
	needsSetup := []string{}
	for i, provider := range providers {
		if !ready[i] {
			needsSetup = append(needsSetup, provider)
		}
	}
	for _, p := range configs {
		if !slices.Contains(needsSetup, p.Provider) {
			kept = append(kept, p)
		}
	}
	return kept, needsSetup
}

func (s *Service) read(ctx context.Context, config cloudscan.Project) Project {
	project := Project{
		Provider:  config.Provider,
		TeamID:    config.TeamID,
		ID:        config.ID,
		Name:      cmp.Or(config.Name, filepath.Base(config.Directory)),
		Directory: config.Directory,
	}
	if config.ID == "" {
		return project
	}
	switch config.Provider {
	case cloudscan.ProviderVercel:
		remote, err := s.vercel(ctx, config.TeamID, config.ID)
		if errors.Is(err, vercel.ErrProjectNotFound) {
			project.Error = "Vercel has no project with this id; link the folder again"
			return project
		}
		if err != nil {
			project.Error = brief(err)
			return project
		}
		project.Name, project.URL = cmp.Or(remote.Name, project.Name), remote.URL
		project.Deployment = vercelDeployment(remote.Latest)
	case cloudscan.ProviderFly:
		app, err := s.fly(ctx, config.ID)
		if err != nil {
			project.Error = brief(err)
			return project
		}
		project.URL = app.URL
		project.Deployment = flyDeployment(app.Latest)
		project.Machines = app.Machines
	case cloudscan.ProviderCloudflare:
		worker, err := s.worker(ctx, config.ID, config.TeamID)
		if err != nil {
			project.Error = brief(err)
			return project
		}
		project.URL = worker.URL
		project.Deployment = workerDeployment(worker.Latest)
	}
	return project
}

func vercelDeployment(d *vercel.Deployment) *Deployment {
	if d == nil {
		return nil
	}
	state, detail := strings.ToLower(d.State), d.Commit
	switch d.State {
	case "ERROR":
		state, detail = "build_failed", d.Reason
	case "READY":
		state = "deployed"
	}
	return &Deployment{State: state, Detail: detail, At: d.At}
}

func flyDeployment(r *fly.Release) *Deployment {
	if r == nil {
		return nil
	}
	state := r.Status
	switch r.Status {
	case "failed":
		state = "deploy_failed"
	case "complete":
		state = "deployed"
	case "running":
		state = "deploying"
	}
	return &Deployment{State: state, Detail: "v" + strconv.Itoa(r.Version), At: r.At}
}

func workerDeployment(d *cloudflare.Deployment) *Deployment {
	if d == nil {
		return nil
	}
	return &Deployment{State: "deployed", Detail: d.VersionID[:min(len(d.VersionID), 8)], At: d.At}
}

// brief is an error's first line, short enough for a card.
func brief(err error) string {
	line, _, _ := strings.Cut(err.Error(), "\n")
	if len(line) > 200 {
		line = line[:200] + "…"
	}
	return line
}
