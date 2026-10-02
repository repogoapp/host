package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/repogo/host/internal/clitool"
)

// Worker is what a project card shows for a Worker: its URL and latest
// deployment, nil before the first. Cloudflare keeps only uploads that
// succeeded, so a deployment never failed.
type Worker struct {
	URL    string
	Latest *Deployment
}

type Deployment struct {
	VersionID string
	At        time.Time
}

// ErrAccountUnknown means wrangler is signed in to several accounts and the
// Worker's config doesn't say which one it belongs to.
var ErrAccountUnknown = errors.New("this Cloudflare login has several accounts; set account_id in the Worker's wrangler config")

// Read reads a Worker's deployments with wrangler, and its address from
// Cloudflare's API with wrangler's own token, which is never kept.
func Read(ctx context.Context, name, accountID string) (Worker, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return readWith(ctx, name, accountID, runCLI, getAPI)
}

type (
	runFunc func(ctx context.Context, env []string, args []string) ([]byte, error)
	getFunc func(ctx context.Context, token, path string) ([]byte, error)
)

func readWith(ctx context.Context, name, accountID string, run runFunc, get getFunc) (Worker, error) {
	if accountID == "" {
		var err error
		if accountID, err = onlyAccount(ctx, run); err != nil {
			return Worker{}, err
		}
	}
	type answer struct {
		url string
		err error
	}
	address := make(chan answer, 1)
	go func() {
		u, err := workerURL(ctx, name, accountID, run, get)
		address <- answer{u, err}
	}()
	out, err := run(ctx, []string{"CLOUDFLARE_ACCOUNT_ID=" + accountID}, []string{"deployments", "list", "--name", name, "--json"})
	if err != nil {
		<-address
		return Worker{}, err
	}
	var deployments []struct {
		CreatedOn time.Time `json:"created_on"`
		Versions  []struct {
			VersionID string `json:"version_id"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(out, &deployments); err != nil {
		<-address
		return Worker{}, fmt.Errorf("decode deployments: %w", err)
	}
	result := Worker{}
	for _, d := range deployments {
		if result.Latest != nil && !d.CreatedOn.After(result.Latest.At) {
			continue
		}
		result.Latest = &Deployment{At: d.CreatedOn}
		if len(d.Versions) > 0 {
			result.Latest.VersionID = d.Versions[0].VersionID
		}
	}
	// The address is a nicety: a card without one still shows its deploys.
	if a := <-address; a.err == nil {
		result.URL = a.url
	}
	return result, nil
}

func onlyAccount(ctx context.Context, run runFunc) (string, error) {
	out, err := run(ctx, nil, []string{"whoami", "--json"})
	if err != nil {
		return "", err
	}
	var me struct {
		Accounts []struct {
			ID string `json:"id"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(out, &me); err != nil {
		return "", fmt.Errorf("decode whoami: %w", err)
	}
	if len(me.Accounts) != 1 {
		return "", ErrAccountUnknown
	}
	return me.Accounts[0].ID, nil
}

// workerURL is the Worker's custom domain, preferring the bare one over www,
// else its workers.dev address.
func workerURL(ctx context.Context, name, accountID string, run runFunc, get getFunc) (string, error) {
	out, err := run(ctx, nil, []string{"auth", "token", "--json"})
	if err != nil {
		return "", err
	}
	var auth struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(out, &auth) != nil || auth.Token == "" {
		return "", errors.New("wrangler gave no token")
	}
	account := "/accounts/" + url.PathEscape(accountID)
	var domains struct {
		Result []struct {
			Hostname string `json:"hostname"`
		} `json:"result"`
	}
	if body, err := get(ctx, auth.Token, account+"/workers/domains?service="+url.QueryEscape(name)); err == nil && json.Unmarshal(body, &domains) == nil {
		found := ""
		for _, d := range domains.Result {
			if !strings.HasPrefix(d.Hostname, "www.") {
				return "https://" + d.Hostname, nil
			}
			if found == "" {
				found = d.Hostname
			}
		}
		if found != "" {
			return "https://" + found, nil
		}
	}
	body, err := get(ctx, auth.Token, account+"/workers/subdomain")
	if err != nil {
		return "", err
	}
	var subdomain struct {
		Result struct {
			Subdomain string `json:"subdomain"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &subdomain); err != nil || subdomain.Result.Subdomain == "" {
		return "", errors.New("no workers.dev subdomain")
	}
	return "https://" + name + "." + subdomain.Result.Subdomain + ".workers.dev", nil
}

func getAPI(ctx context.Context, token, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.cloudflare.com/client/v4"+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("cloudflare api: %s", resp.Status)
	}
	return body, nil
}

func runCLI(ctx context.Context, env []string, args []string) ([]byte, error) {
	return runCLIInput(ctx, env, args, nil)
}

// runCLIInput hands stdin to wrangler, so a secret's value never shows in
// the process list.
func runCLIInput(ctx context.Context, env []string, args []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, clitool.PreferredExecutable(Tool().Spec), args...)
	cmd.Dir = os.TempDir()
	cmd.Env = append(append(os.Environ(), quiet), env...)
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
		if len(diagnostic) > 2048 {
			diagnostic = diagnostic[:2048]
		}
		return nil, fmt.Errorf("wrangler %s: %s", args[0], diagnostic)
	}
	return nil, fmt.Errorf("wrangler %s: %w", args[0], err)
}
