// Package vercel is the Vercel CLI on this host: installing it, and reading
// the projects linked under a folder for their cards.
package vercel

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/repogo/host/internal/errkind"
)

// ErrProjectNotFound is Vercel answering 404 for a linked project id.
var ErrProjectNotFound = errkind.New(errkind.NotFound, "vercel: project not found")

// Project is what a project card shows: its name, framework, production URL
// and the latest production deployment, nil before the first one.
type Project struct {
	Name      string
	Framework string
	URL       string
	Latest    *Deployment
}

// Deployment is one production deployment. State is Vercel's readyState
// (READY, ERROR, BUILDING, CANCELED…); Reason is set only for ERROR.
type Deployment struct {
	ID      string
	State   string
	At      time.Time
	Commit  string
	Message string
	Reason  string
}

// Read reads one linked project by id, and the build log of its latest
// production deployment when that one failed.
func Read(ctx context.Context, teamID, projectID string) (Project, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return readWith(ctx, teamID, projectID, runCLI)
}

// readWith costs one request, or two for a failed deployment: Vercel's
// project record already carries its latest deployments.
func readWith(ctx context.Context, teamID, projectID string, run func(context.Context, []string) ([]byte, error)) (Project, error) {
	endpoint := "/v9/projects/" + url.PathEscape(projectID) + "?teamId=" + url.QueryEscape(teamID)
	out, err := run(ctx, []string{"api", endpoint, "--raw", "--non-interactive"})
	if err != nil {
		return Project{}, err
	}
	var raw struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Framework string `json:"framework"`
		Targets   struct {
			Production struct {
				Alias []string `json:"alias"`
			} `json:"production"`
		} `json:"targets"`
		LatestDeployments []struct {
			ID         string            `json:"id"`
			ReadyState string            `json:"readyState"`
			Target     *string           `json:"target"`
			CreatedAt  int64             `json:"createdAt"`
			ReadyAt    int64             `json:"readyAt"`
			Meta       map[string]string `json:"meta"`
		} `json:"latestDeployments"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return Project{}, fmt.Errorf("decode project: %w", err)
	}
	if raw.ID == "" {
		return Project{}, fmt.Errorf("CLI project missing id")
	}
	project := Project{Name: raw.Name, Framework: raw.Framework, URL: productionURL(raw.Targets.Production.Alias)}
	for _, d := range raw.LatestDeployments {
		if d.Target == nil || *d.Target != "production" {
			continue
		}
		at := d.CreatedAt
		if d.ReadyState == "READY" && d.ReadyAt > 0 {
			at = d.ReadyAt
		}
		if project.Latest == nil || at > project.Latest.At.UnixMilli() {
			project.Latest = &Deployment{ID: d.ID, State: d.ReadyState, At: time.UnixMilli(at),
				Commit: shortCommit(d.Meta), Message: commitMessage(d.Meta)}
		}
	}
	if project.Latest != nil && project.Latest.State == "ERROR" {
		project.Latest.Reason = failureReason(ctx, teamID, project.Latest.ID, run)
	}
	return project, nil
}

// shortCommit reads the git provider's sha, or `gitCommitSha`, which a deploy
// from the CLI carries instead.
func shortCommit(meta map[string]string) string {
	sha := cmp.Or(meta["githubCommitSha"], meta["gitlabCommitSha"], meta["bitbucketCommitSha"], meta["gitCommitSha"])
	return sha[:min(len(sha), 7)]
}

// commitMessage is the commit's first line, cut short: the card shows one line.
func commitMessage(meta map[string]string) string {
	message := cmp.Or(meta["githubCommitMessage"], meta["gitlabCommitMessage"], meta["bitbucketCommitMessage"], meta["gitCommitMessage"])
	message, _, _ = strings.Cut(message, "\n")
	message = strings.TrimSpace(message)
	if r := []rune(message); len(r) > 80 {
		message = string(r[:79]) + "…"
	}
	return message
}

// failureReason is the first error line near the end of the build log. The
// very last one is Vercel's generic `Command "…" exited with 1`, so it comes
// only when nothing better is there; a log that can't be read gives "".
func failureReason(ctx context.Context, teamID, deploymentID string, run func(context.Context, []string) ([]byte, error)) string {
	endpoint := "/v3/deployments/" + url.PathEscape(deploymentID) + "/events?teamId=" + url.QueryEscape(teamID) + "&direction=backward&limit=50"
	out, err := run(ctx, []string{"api", endpoint, "--raw", "--non-interactive"})
	if err != nil {
		return ""
	}
	var events []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(out, &events) != nil {
		return ""
	}
	generic := ""
	for i := len(events) - 1; i >= 0; i-- {
		line := strings.TrimSpace(events[i].Text)
		if events[i].Type != "stderr" || !strings.HasPrefix(strings.ToLower(line), "error") {
			continue
		}
		if strings.Contains(line, "exited with") {
			generic = cmp.Or(generic, line)
			continue
		}
		return line
	}
	return generic
}

// productionURL prefers the first custom domain over the generated
// *.vercel.app ones, which Vercel lists alongside it.
func productionURL(aliases []string) string {
	for _, alias := range aliases {
		if !strings.HasSuffix(alias, ".vercel.app") {
			return "https://" + alias
		}
	}
	if len(aliases) > 0 {
		return "https://" + aliases[0]
	}
	return ""
}

func runCLI(ctx context.Context, args []string) ([]byte, error) {
	return runCLIInput(ctx, args, nil)
}

// runCLIInput hands stdin to the CLI, so a value it writes never shows in
// the process list.
func runCLIInput(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "vercel", args...)
	cmd.Dir = os.TempDir()
	cmd.WaitDelay = time.Second
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if exit, ok := err.(*exec.ExitError); ok {
		diagnostic := strings.TrimSpace(string(exit.Stderr))
		// The CLI reports the API status only in its message: "Error: Project not found. (404)".
		if strings.HasSuffix(diagnostic, "(404)") {
			return nil, ErrProjectNotFound
		}
		if len(diagnostic) > 2048 {
			diagnostic = diagnostic[:2048]
		}
		return nil, fmt.Errorf("vercel %s: %w: %s", args[0], err, diagnostic)
	}
	return nil, fmt.Errorf("vercel %s: %w", args[0], err)
}
