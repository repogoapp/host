package vercel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"time"

	"github.com/repogo/host/internal/errkind"
)

// EnvTargets are the environments a Vercel variable can be set for.
var EnvTargets = []string{"production", "preview", "development"}

// EnvVar is one of a project's variables, without its value. A key can have
// several, one per set of targets or branch, so ID is what names one.
type EnvVar struct {
	ID        string
	Key       string
	Targets   []string
	GitBranch string
	Sensitive bool
}

// EnvWrite creates a variable when ID is empty, else changes that one. An
// empty Value on a change keeps the current value.
type EnvWrite struct {
	TeamID    string
	ProjectID string
	ID        string
	Key       string
	Value     string
	Targets   []string
	GitBranch string
	Sensitive bool
}

type inputRunner func(ctx context.Context, args []string, stdin []byte) ([]byte, error)

// rawEnv is Vercel's record; Type is encrypted, plain, sensitive, secret or system.
type rawEnv struct {
	ID        string          `json:"id"`
	Key       string          `json:"key"`
	Value     string          `json:"value"`
	Type      string          `json:"type"`
	Target    json.RawMessage `json:"target"`
	GitBranch *string         `json:"gitBranch"`
}

func (r rawEnv) envVar() EnvVar {
	v := EnvVar{ID: r.ID, Key: r.Key, Sensitive: r.Type == "sensitive" || r.Type == "secret", Targets: []string{}}
	if r.GitBranch != nil {
		v.GitBranch = *r.GitBranch
	}
	// Older records send one target as a string instead of a list.
	if json.Unmarshal(r.Target, &v.Targets) != nil || v.Targets == nil {
		var one string
		v.Targets = []string{}
		if json.Unmarshal(r.Target, &one) == nil && one != "" {
			v.Targets = []string{one}
		}
	}
	return v
}

// ListEnv reads a project's variables without their values. Vercel's own
// system variables are left out: nobody can change them.
func ListEnv(ctx context.Context, teamID, projectID string) ([]EnvVar, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return listEnvWith(ctx, teamID, projectID, runCLIInput)
}

func listEnvWith(ctx context.Context, teamID, projectID string, run inputRunner) ([]EnvVar, error) {
	path, err := envPath("/v10", teamID, projectID, "")
	if err != nil {
		return nil, err
	}
	out, err := run(ctx, apiArgs("GET", path), nil)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Envs []rawEnv `json:"envs"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("decode env list: %w", err)
	}
	vars := make([]EnvVar, 0, len(raw.Envs))
	for _, e := range raw.Envs {
		if e.Type != "system" {
			vars = append(vars, e.envVar())
		}
	}
	return vars, nil
}

// EnvValue reads one variable's value; Vercel never returns a sensitive one.
func EnvValue(ctx context.Context, teamID, projectID, id string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return envValueWith(ctx, teamID, projectID, id, runCLIInput)
}

func envValueWith(ctx context.Context, teamID, projectID, id string, run inputRunner) (string, error) {
	if id == "" {
		return "", fmt.Errorf("%w: id is required", errkind.ErrInvalid)
	}
	path, err := envPath("/v1", teamID, projectID, id)
	if err != nil {
		return "", err
	}
	out, err := run(ctx, apiArgs("GET", path), nil)
	if err != nil {
		return "", err
	}
	var raw rawEnv
	if err := json.Unmarshal(out, &raw); err != nil {
		return "", fmt.Errorf("decode env value: %w", err)
	}
	if raw.envVar().Sensitive {
		return "", fmt.Errorf("%w: a sensitive variable can't be read back", errkind.ErrInvalid)
	}
	return raw.Value, nil
}

// SetEnv creates or changes one variable, sending it on stdin.
func SetEnv(ctx context.Context, w EnvWrite) (EnvVar, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return setEnvWith(ctx, w, runCLIInput)
}

func setEnvWith(ctx context.Context, w EnvWrite, run inputRunner) (EnvVar, error) {
	if err := checkEnvWrite(w); err != nil {
		return EnvVar{}, err
	}
	body := map[string]any{"key": w.Key}
	if w.Value != "" || w.ID == "" {
		body["value"] = w.Value
	}
	if len(w.Targets) > 0 {
		body["target"] = w.Targets
		// A change that sends targets also clears a branch it no longer has.
		if w.GitBranch != "" {
			body["gitBranch"] = w.GitBranch
		} else if w.ID != "" {
			body["gitBranch"] = nil
		}
	}
	method, version := "PATCH", "/v9"
	if w.ID == "" {
		method, version = "POST", "/v10"
		body["type"] = "encrypted"
	}
	// A change only ever makes a variable sensitive: Vercel can't undo it.
	if w.Sensitive {
		body["type"] = "sensitive"
	}
	path, err := envPath(version, w.TeamID, w.ProjectID, w.ID)
	if err != nil {
		return EnvVar{}, err
	}
	in, err := json.Marshal(body)
	if err != nil {
		return EnvVar{}, err
	}
	out, err := run(ctx, append(apiArgs(method, path), "--input", "-"), in)
	if err != nil {
		return EnvVar{}, err
	}
	return decodeWritten(out)
}

// decodeWritten reads a PATCH's record, or a POST's {created}, which is one
// record or a list of them.
func decodeWritten(out []byte) (EnvVar, error) {
	var created struct {
		Created json.RawMessage `json:"created"`
	}
	if err := json.Unmarshal(out, &created); err != nil {
		return EnvVar{}, fmt.Errorf("decode env: %w", err)
	}
	if len(created.Created) == 0 {
		var raw rawEnv
		if err := json.Unmarshal(out, &raw); err != nil {
			return EnvVar{}, fmt.Errorf("decode env: %w", err)
		}
		return raw.envVar(), nil
	}
	var one rawEnv
	if json.Unmarshal(created.Created, &one) == nil {
		return one.envVar(), nil
	}
	var many []rawEnv
	if err := json.Unmarshal(created.Created, &many); err != nil || len(many) == 0 {
		return EnvVar{}, fmt.Errorf("decode created env: %w", err)
	}
	return many[0].envVar(), nil
}

// RemoveEnv deletes one variable. The phone asks before calling, and the CLI
// refuses a DELETE without a terminal unless told it was confirmed.
func RemoveEnv(ctx context.Context, teamID, projectID, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return removeEnvWith(ctx, teamID, projectID, id, runCLIInput)
}

func removeEnvWith(ctx context.Context, teamID, projectID, id string, run inputRunner) error {
	if id == "" {
		return fmt.Errorf("%w: id is required", errkind.ErrInvalid)
	}
	path, err := envPath("/v9", teamID, projectID, id)
	if err != nil {
		return err
	}
	_, err = run(ctx, append(apiArgs("DELETE", path), "--dangerously-skip-permissions"), nil)
	return err
}

// RedeployProduction rebuilds the latest production deployment, which is
// how a production variable reaches the running site.
func RedeployProduction(ctx context.Context, teamID, projectID string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return redeployWith(ctx, teamID, projectID, runCLIInput)
}

func redeployWith(ctx context.Context, teamID, projectID string, run inputRunner) error {
	read := func(ctx context.Context, args []string) ([]byte, error) { return run(ctx, args, nil) }
	page, err := readDeploymentsWith(ctx, DeploymentQuery{TeamID: teamID, ProjectID: projectID, Environment: "production", Limit: 1}, read)
	if err != nil {
		return err
	}
	if len(page.Deployments) == 0 {
		return fmt.Errorf("%w: the project has no production deployment to redeploy", errkind.ErrInvalid)
	}
	_, err = run(ctx, []string{"redeploy", page.Deployments[0].ID, "--target", "production", "--no-wait",
		"--scope", teamID, "--non-interactive"}, nil)
	return err
}

// checkEnvWrite applies Vercel's rules before the call, so the phone gets a
// clear message instead of the API's 400.
func checkEnvWrite(w EnvWrite) error {
	if w.ID == "" && len(w.Targets) == 0 {
		return fmt.Errorf("%w: targets is required", errkind.ErrInvalid)
	}
	for _, t := range w.Targets {
		if !slices.Contains(EnvTargets, t) {
			return fmt.Errorf("%w: targets are production, preview or development", errkind.ErrInvalid)
		}
	}
	if w.Sensitive && slices.Contains(w.Targets, "development") {
		return fmt.Errorf("%w: a development variable can't be sensitive", errkind.ErrInvalid)
	}
	if w.GitBranch != "" && !slices.Equal(w.Targets, []string{"preview"}) {
		return fmt.Errorf("%w: a git branch needs the preview target alone", errkind.ErrInvalid)
	}
	return nil
}

func apiArgs(method, path string) []string {
	return []string{"api", path, "-X", method, "--raw", "--non-interactive"}
}

func envPath(version, teamID, projectID, id string) (string, error) {
	if teamID == "" || projectID == "" {
		return "", fmt.Errorf("%w: team_id and project_id are required", errkind.ErrInvalid)
	}
	path := version + "/projects/" + url.PathEscape(projectID) + "/env"
	if id != "" {
		path += "/" + url.PathEscape(id)
	}
	return path + "?teamId=" + url.QueryEscape(teamID), nil
}
