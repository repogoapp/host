package antigravity

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/repogoapp/host/internal/binfetch"
	"github.com/repogoapp/host/internal/clitool"
)

// manifestBase is the updater agy itself asks for its newest build, one
// manifest per system: <os>_<arch>.json.
const manifestBase = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests"

// downloads is where every build the manifest names must come from.
const downloads = "https://storage.googleapis.com/antigravity-public/antigravity-cli/"

var (
	semver  = regexp.MustCompile(`^\d+\.\d+\.\d+$`)
	sha512s = regexp.MustCompile(`^[0-9a-fA-F]{128}$`)
)

// build is one manifest: the newest version, its archive and the archive's
// SHA-512.
type build struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA512  string `json:"sha512"`
}

// newest reads the manifest for a system. Windows' build is a bare .exe,
// which the package installer doesn't take.
func newest(ctx context.Context, goos, goarch string) (build, error) {
	target, err := target(goos, goarch)
	if err != nil {
		return build{}, err
	}
	raw, err := binfetch.Fetch(ctx, manifestBase+"/"+target+".json")
	if err != nil {
		return build{}, err
	}
	return parseManifest(raw, target)
}

// parseManifest reads a manifest, refusing one that names a build from
// anywhere but Google's download bucket or without a SHA-512.
func parseManifest(raw []byte, target string) (build, error) {
	var b build
	if err := json.Unmarshal(raw, &b); err != nil {
		return build{}, fmt.Errorf("antigravity manifest: %w", err)
	}
	if !semver.MatchString(b.Version) || !sha512s.MatchString(b.SHA512) ||
		!strings.HasPrefix(b.URL, downloads+b.Version+"-") || !strings.HasSuffix(b.URL, ".tar.gz") {
		return build{}, fmt.Errorf("antigravity manifest for %s names an unexpected build", target)
	}
	return b, nil
}

// target is the manifest's name for a system; Linux on musl has its own.
func target(goos, goarch string) (string, error) {
	arch := map[string]string{"arm64": "arm64", "amd64": "amd64"}[goarch]
	if arch == "" || (goos != "darwin" && goos != "linux") {
		return "", fmt.Errorf("%w: agy on %s/%s", binfetch.ErrUnsupported, goos, goarch)
	}
	t := goos + "_" + arch
	if goos == "linux" && musl() {
		t += "_musl"
	}
	return t, nil
}

// musl reports a Linux whose C library is musl (Alpine).
func musl() bool {
	matches, _ := filepath.Glob("/lib/ld-musl-*")
	return len(matches) > 0
}

// release is agy's build for a system, unpacked under
// ~/.local/share/antigravity-cli/<version>: one binary, antigravity, which
// ~/.local/bin/agy links.
func release(ctx context.Context, goos, goarch string) (clitool.Package, error) {
	b, err := newest(ctx, goos, goarch)
	if err != nil {
		return clitool.Package{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return clitool.Package{URL: b.URL}, err
	}
	return clitool.Package{
		URL: b.URL, SHA256: b.SHA512,
		Dir:   filepath.Join(home, ".local", "share", "antigravity-cli", b.Version),
		Entry: "antigravity",
	}, nil
}

// latestVersion is the manifest's version for this system.
func latestVersion(ctx context.Context) (string, error) {
	b, err := newest(ctx, runtime.GOOS, runtime.GOARCH)
	return b.Version, err
}
