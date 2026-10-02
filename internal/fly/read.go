package fly

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/repogo/host/internal/clitool"
)

// App keeps deployment and machine state separate on the card.
type App struct {
	URL      string
	Machines *MachineSummary
	Latest   *Release
}

// Release is one deploy. Status is Fly's own (complete, failed, running…).
type Release struct {
	Version int
	Status  string
	At      time.Time
}

// Read reads releases, certificates and machine summaries in parallel.
func Read(ctx context.Context, app string) (App, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return readWith(ctx, app, runCLI)
}

func readWith(ctx context.Context, app string, run func(context.Context, []string) ([]byte, error)) (App, error) {
	type answer struct {
		out []byte
		err error
	}
	machines := make(chan *MachineSummary, 1)
	go func() { machines <- readMachines(ctx, app, run) }()
	certs := make(chan answer, 1)
	go func() {
		out, err := run(ctx, []string{"certs", "list", "-a", app, "--json"})
		certs <- answer{out, err}
	}()
	out, err := run(ctx, []string{"releases", "-a", app, "--json"})
	if err != nil {
		<-certs
		<-machines
		return App{}, err
	}
	var releases []struct {
		Version   int       `json:"Version"`
		Status    string    `json:"Status"`
		CreatedAt time.Time `json:"CreatedAt"`
	}
	if err := json.Unmarshal(out, &releases); err != nil {
		<-certs
		<-machines
		return App{}, fmt.Errorf("decode releases: %w", err)
	}
	result := App{URL: "https://" + app + ".fly.dev"}
	for _, r := range releases {
		if result.Latest == nil || r.Version > result.Latest.Version {
			result.Latest = &Release{Version: r.Version, Status: r.Status, At: r.CreatedAt}
		}
	}
	// A custom domain is the one people use; without a certificate, the
	// app's fly.dev address is.
	if c := <-certs; c.err == nil {
		var hosts []cert
		if json.Unmarshal(c.out, &hosts) == nil {
			if host := customDomain(hosts); host != "" {
				result.URL = "https://" + host
			}
		}
	}
	result.Machines = <-machines
	return result, nil
}

// cert is one hostname `fly certs list` reports for the app.
type cert struct {
	Hostname string `json:"hostname"`
	Status   string `json:"status"`
}

// customDomain is the first ready hostname, preferring the bare domain over
// its www twin, since both usually point at the same app.
func customDomain(hosts []cert) string {
	found := ""
	for _, h := range hosts {
		if !strings.EqualFold(h.Status, "ready") {
			continue
		}
		if !strings.HasPrefix(h.Hostname, "www.") {
			return h.Hostname
		}
		if found == "" {
			found = h.Hostname
		}
	}
	return found
}

func runCLI(ctx context.Context, args []string) ([]byte, error) {
	return runCLIInput(ctx, args, nil)
}

// runCLIInput hands stdin to flyctl, so a secret's value never shows in the
// process list.
func runCLIInput(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, clitool.PreferredExecutable(Tool().Spec), args...)
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
		if len(diagnostic) > 2048 {
			diagnostic = diagnostic[:2048]
		}
		return nil, fmt.Errorf("flyctl %s: %s", args[0], diagnostic)
	}
	return nil, fmt.Errorf("flyctl %s: %w", args[0], err)
}
