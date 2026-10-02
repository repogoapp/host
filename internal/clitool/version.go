package clitool

import (
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/repogo/host/internal/binfetch"
)

// versionPattern is deliberately loose. Every CLI prints its version
// differently — "1.2.3", "codex-cli 1.2.3", "1.2.3 (Claude Code)" — and the
// only stable part is that a semver appears somewhere in the first line.
var versionPattern = regexp.MustCompile(`\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.\-]+)?`)

type versionEntry struct {
	size    int64
	modTime time.Time
	version string
}

// version is the binary's own `--version`, kept while the file at path is the
// same one: an install or update replaces it, which changes its size or time.
func (in *Inventory) version(ctx context.Context, path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return probeVersion(ctx, path)
	}
	in.mu.Lock()
	e, ok := in.versions[path]
	in.mu.Unlock()
	if ok && e.size == info.Size() && e.modTime.Equal(info.ModTime()) {
		return e.version
	}
	version := probeVersion(ctx, path)
	if version != "" {
		in.mu.Lock()
		in.versions[path] = versionEntry{size: info.Size(), modTime: info.ModTime(), version: version}
		in.mu.Unlock()
	}
	return version
}

func probeVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil && len(out) == 0 {
		return ""
	}
	return versionPattern.FindString(string(out))
}

var registryClient = &http.Client{Timeout: latestTimeout}

// latestVersion asks the npm registry, or GitHub's latest release for a CLI
// not on npm, cached for an hour. Any failure is an empty string: no network
// is a normal condition, not a failed listing.
func (in *Inventory) latestVersion(ctx context.Context, s Spec) string {
	key := cmp.Or(s.Pkg, s.ReleaseRepo)
	if key == "" {
		return ""
	}

	in.mu.Lock()
	if e, ok := in.latest[key]; ok && time.Now().Before(e.expiresAt) {
		in.mu.Unlock()
		return e.version
	}
	in.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, latestTimeout)
	defer cancel()

	version := ""
	if s.Pkg == "" {
		if tag, err := binfetch.Latest(ctx, s.ReleaseRepo); err == nil {
			version = strings.TrimPrefix(tag, "v")
		}
	} else if req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://registry.npmjs.org/"+s.Pkg+"/latest", nil); err == nil {
		req.Header.Set("Accept", "application/json")
		if resp, err := registryClient.Do(req); err == nil {
			defer resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				var body struct {
					Version string `json:"version"`
				}
				if json.NewDecoder(resp.Body).Decode(&body) == nil {
					version = strings.TrimSpace(body.Version)
				}
			}
		}
	}

	// A miss is cached too, briefly, so an offline host does not make the same
	// doomed request on every probe.
	ttl := latestTTL
	if version == "" {
		ttl = time.Minute
	}
	in.mu.Lock()
	in.latest[key] = latestEntry{version: version, expiresAt: time.Now().Add(ttl)}
	in.mu.Unlock()

	return version
}

// compareVersions orders two versions numerically, ignoring prerelease and
// build suffixes; enough for "is the installed one older".
func compareVersions(a, b string) int {
	as, bs := versionParts(a), versionParts(b)
	for i := 0; i < len(as) || i < len(bs); i++ {
		av, bv := 0, 0
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if cut := strings.IndexAny(v, "-+"); cut >= 0 {
		v = v[:cut]
	}
	fields := strings.Split(v, ".")
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			break
		}
		out = append(out, n)
	}
	return out
}
