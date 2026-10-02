package vercel

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/repogo/host/internal/errkind"
)

const (
	defaultDeploymentLimit = 20
	maxDeploymentLimit     = 100
)

// deploymentStates is the wire's state for each of Vercel's readyStates.
var deploymentStates = map[string]string{
	"QUEUED": "queued", "INITIALIZING": "initializing", "BUILDING": "building",
	"READY": "ready", "ERROR": "error", "CANCELED": "canceled",
}

// DeploymentQuery is one page of a project's deployments, newest first.
// Cursor is the last page's NextCursor, or "" for the newest.
type DeploymentQuery struct {
	TeamID      string
	ProjectID   string
	Environment string // production, preview, or "" for both
	State       string // a deploymentStates value, or "" for all
	Cursor      string
	Limit       int
}

// DeploymentSummary is one row of the deployments list. Duration is the
// build's, zero until it finishes; Error is set only for a failed one.
type DeploymentSummary struct {
	ID              string    `json:"id"`
	URL             string    `json:"url"`
	Environment     string    `json:"environment"`
	State           string    `json:"state"`
	At              time.Time `json:"at"`
	DurationSeconds int       `json:"duration_seconds"`
	Branch          string    `json:"branch"`
	Commit          string    `json:"commit"`
	Message         string    `json:"message"`
	Author          string    `json:"author"`
	Error           string    `json:"error"`
}

// DeploymentPage is a page of deployments; NextCursor is "" on the last.
type DeploymentPage struct {
	Deployments []DeploymentSummary `json:"deployments" wire:"array"`
	NextCursor  string              `json:"next_cursor"`
}

// ReadDeployments reads one page with Vercel's deployments API.
func ReadDeployments(ctx context.Context, q DeploymentQuery) (DeploymentPage, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return readDeploymentsWith(ctx, q, runCLI)
}

func readDeploymentsWith(ctx context.Context, q DeploymentQuery, run func(context.Context, []string) ([]byte, error)) (DeploymentPage, error) {
	endpoint, err := deploymentsEndpoint(q)
	if err != nil {
		return DeploymentPage{}, err
	}
	out, err := run(ctx, []string{"api", endpoint, "--raw", "--non-interactive"})
	if err != nil {
		return DeploymentPage{}, err
	}
	var raw struct {
		Deployments []struct {
			UID          string                    `json:"uid"`
			URL          string                    `json:"url"`
			Target       *string                   `json:"target"`
			ReadyState   string                    `json:"readyState"`
			Created      int64                     `json:"created"`
			BuildingAt   int64                     `json:"buildingAt"`
			Ready        int64                     `json:"ready"`
			Meta         map[string]string         `json:"meta"`
			Creator      struct{ Username string } `json:"creator"`
			ErrorMessage string                    `json:"errorMessage"`
		} `json:"deployments"`
		Pagination struct {
			Next *int64 `json:"next"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return DeploymentPage{}, fmt.Errorf("decode deployments: %w", err)
	}
	page := DeploymentPage{Deployments: make([]DeploymentSummary, 0, len(raw.Deployments))}
	for _, d := range raw.Deployments {
		row := DeploymentSummary{
			ID:          d.UID,
			URL:         "https://" + d.URL,
			Environment: "preview",
			State:       cmp.Or(deploymentStates[d.ReadyState], strings.ToLower(d.ReadyState)),
			At:          time.UnixMilli(d.Created),
			Branch:      cmp.Or(d.Meta["githubCommitRef"], d.Meta["gitlabCommitRef"], d.Meta["bitbucketCommitRef"], d.Meta["gitCommitRef"]),
			Commit:      shortCommit(d.Meta),
			Message:     commitMessage(d.Meta),
			Author:      cmp.Or(d.Meta["githubCommitAuthorLogin"], d.Meta["githubCommitAuthorName"], d.Meta["gitlabCommitAuthorName"], d.Meta["gitCommitAuthorName"], d.Creator.Username),
		}
		if d.Target != nil && *d.Target == "production" {
			row.Environment = "production"
		}
		if d.ReadyState == "READY" && d.BuildingAt > 0 && d.Ready >= d.BuildingAt {
			row.DurationSeconds = int((d.Ready - d.BuildingAt) / 1000)
		}
		if d.ReadyState == "ERROR" {
			row.Error = d.ErrorMessage
		}
		page.Deployments = append(page.Deployments, row)
	}
	// Vercel's next is null on the last page.
	if raw.Pagination.Next != nil {
		page.NextCursor = strconv.FormatInt(*raw.Pagination.Next, 10)
	}
	return page, nil
}

// deploymentsEndpoint checks the query and builds the API path. The cursor
// is Vercel's own: the creation time, in milliseconds, to read before.
func deploymentsEndpoint(q DeploymentQuery) (string, error) {
	if !vercelID.MatchString(q.TeamID) || !vercelID.MatchString(q.ProjectID) {
		return "", fmt.Errorf("%w: team_id and project_id are required", errkind.ErrInvalid)
	}
	if !slices.Contains([]string{"", "production", "preview"}, q.Environment) {
		return "", fmt.Errorf("%w: environment is production or preview", errkind.ErrInvalid)
	}
	values := url.Values{"projectId": {q.ProjectID}, "teamId": {q.TeamID}}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultDeploymentLimit
	}
	values.Set("limit", strconv.Itoa(min(limit, maxDeploymentLimit)))
	if q.Environment != "" {
		values.Set("target", q.Environment)
	}
	if q.State != "" {
		state := ""
		for vercelState, wire := range deploymentStates {
			if wire == q.State {
				state = vercelState
			}
		}
		if state == "" {
			return "", fmt.Errorf("%w: state is queued, initializing, building, ready, error or canceled", errkind.ErrInvalid)
		}
		values.Set("state", state)
	}
	if q.Cursor != "" {
		if _, err := strconv.ParseInt(q.Cursor, 10, 64); err != nil {
			return "", fmt.Errorf("%w: cursor is a next_cursor from an earlier page", errkind.ErrInvalid)
		}
		values.Set("until", q.Cursor)
	}
	return "/v6/deployments?" + values.Encode(), nil
}
