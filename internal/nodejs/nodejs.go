// Package nodejs makes sure the host has a Node new enough for npm and npx:
// installing agent CLIs, the pairing code, and MCP servers agents start. The
// machine's own when it has one, else the current LTS in the host's state.
package nodejs

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/binfetch"
)

// minMajor is the oldest Node the host's npm and npx uses run on.
const minMajor = 20

// Dist is nodejs.org's release tree; tests point it at a local server.
var Dist = "https://nodejs.org/dist"

// Ensure returns the bin directory of the host's own Node for PATH, or "" when
// the machine's node is new enough. It downloads only when neither will do.
func Ensure(ctx context.Context) (string, error) {
	if major(ctx, "node") >= minMajor {
		return "", nil
	}
	dir, err := apphome.Path("node")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "bin")
	if major(ctx, filepath.Join(bin, "node")) >= minMajor {
		return bin, nil
	}

	version, err := latestLTS(ctx)
	if err != nil {
		return "", err
	}
	name, err := asset(version, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	sum, err := checksum(ctx, version, name)
	if err != nil {
		return "", err
	}
	if err := binfetch.InstallTree(ctx, Dist+"/"+version+"/"+name, sum, dir); err != nil {
		return "", err
	}
	if major(ctx, filepath.Join(bin, "node")) < minMajor {
		return "", fmt.Errorf("nodejs: installed %s but it does not run here", version)
	}
	return bin, nil
}

// major is `node -v`'s major version, 0 when it is missing or will not run.
func major(ctx context.Context, node string) int {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "-v").Output()
	if err != nil {
		return 0
	}
	v, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(string(out)), "v"), ".")
	n, _ := strconv.Atoi(v)
	return n
}

// latestLTS is the newest LTS release, e.g. "v24.21.0"; index.json lists
// newest first, with lts false or the release line's name.
func latestLTS(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	index, err := binfetch.Fetch(ctx, Dist+"/index.json")
	if err != nil {
		return "", err
	}
	var releases []struct {
		Version string `json:"version"`
		LTS     any    `json:"lts"`
	}
	if err := json.Unmarshal(index, &releases); err != nil {
		return "", fmt.Errorf("nodejs: unreadable release index: %w", err)
	}
	for _, r := range releases {
		if lts, ok := r.LTS.(string); ok && lts != "" {
			return r.Version, nil
		}
	}
	return "", fmt.Errorf("nodejs: no LTS release in the index")
}

// checksum is name's sha256 from the release's SHASUMS256.txt, whose lines
// read "<hex>  <file>".
func checksum(ctx context.Context, version, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	sums, err := binfetch.Fetch(ctx, Dist+"/"+version+"/SHASUMS256.txt")
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(string(sums)) {
		if sum, file, ok := strings.Cut(strings.TrimSpace(line), "  "); ok && file == name {
			return sum, nil
		}
	}
	return "", fmt.Errorf("nodejs: no checksum for %s", name)
}

// asset is the release archive for a system: node-v24.21.0-linux-arm64.tar.gz.
func asset(version, goos, goarch string) (string, error) {
	arch := map[string]string{"arm64": "arm64", "amd64": "x64"}[goarch]
	if arch == "" || (goos != "linux" && goos != "darwin") {
		return "", fmt.Errorf("%w: node on %s/%s", binfetch.ErrUnsupported, goos, goarch)
	}
	return "node-" + version + "-" + goos + "-" + arch + ".tar.gz", nil
}
