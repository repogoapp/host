// Package vercel exposes a Vercel project's request logs and deployments
// through the host's Vercel CLI sign-in.
package vercel

import (
	"context"
	"time"

	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/vercel"
)

// Deps is how the family reads Vercel; the host passes vercel.ReadLogs and
// vercel.ReadDeployments.
type Deps struct {
	ReadLogs        func(context.Context, vercel.LogQuery) (vercel.LogPage, error)
	ReadDeployments func(context.Context, vercel.DeploymentQuery) (vercel.DeploymentPage, error)
}

// LogsParams picks a page of a card's request logs, newest first: optional
// filters, each step back covering window_minutes (default 60, at most 1440),
// and the last page's next_cursor.
type LogsParams struct {
	TeamID        string `json:"team_id"`
	ProjectID     string `json:"project_id"`
	Environment   string `json:"environment"`
	Level         string `json:"level"`
	StatusCode    string `json:"status_code"`
	WindowMinutes int    `json:"window_minutes"`
	Cursor        string `json:"cursor"`
	Limit         int    `json:"limit"`
}

// DeploymentsParams picks a page of a card's deployments, newest first:
// optional environment and state filters, and the last page's next_cursor.
type DeploymentsParams struct {
	TeamID      string `json:"team_id"`
	ProjectID   string `json:"project_id"`
	Environment string `json:"environment"`
	State       string `json:"state"`
	Cursor      string `json:"cursor"`
	Limit       int    `json:"limit"`
}

func Register(r *rpc.Router, d Deps) {
	rpc.Add(r, "vercel.logs", d.logs)
	rpc.Add(r, "vercel.deployments", d.deployments)
}

func (d Deps) deployments(ctx context.Context, _ rpc.Caller, p DeploymentsParams) (vercel.DeploymentPage, error) {
	return d.ReadDeployments(ctx, vercel.DeploymentQuery{
		TeamID: p.TeamID, ProjectID: p.ProjectID, Environment: p.Environment, State: p.State, Cursor: p.Cursor, Limit: p.Limit,
	})
}

func (d Deps) logs(ctx context.Context, _ rpc.Caller, p LogsParams) (vercel.LogPage, error) {
	return d.ReadLogs(ctx, vercel.LogQuery{
		TeamID: p.TeamID, ProjectID: p.ProjectID, Environment: p.Environment, Level: p.Level,
		StatusCode: p.StatusCode, Window: time.Duration(p.WindowMinutes) * time.Minute, Cursor: p.Cursor, Limit: p.Limit,
	})
}
