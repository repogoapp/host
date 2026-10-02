// Package release resolves and verifies published host binaries.
package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

var Version = "dev"
var Repository = ""

var client = &http.Client{Timeout: 2 * time.Minute}

type Asset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Version string           `json:"version"`
	Assets  map[string]Asset `json:"assets"`
}

func Latest(ctx context.Context) (Manifest, error) {
	if Repository == "" {
		return Manifest{}, fmt.Errorf("this development build has no release repository")
	}
	var latest struct {
		Tag string `json:"tag_name"`
	}
	if err := getJSON(ctx, "https://api.github.com/repos/"+Repository+"/releases/latest", &latest); err != nil {
		return Manifest{}, err
	}
	if latest.Tag == "" {
		return Manifest{}, fmt.Errorf("release has no tag")
	}
	var manifest Manifest
	err := getJSON(ctx, "https://github.com/"+Repository+"/releases/download/"+latest.Tag+"/manifest.json", &manifest)
	if err == nil && "v"+manifest.Version != latest.Tag {
		err = fmt.Errorf("release manifest version does not match its tag")
	}
	return manifest, err
}

func getJSON(ctx context.Context, url string, into any) error {
	r, err := fetch(ctx, url)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(into)
}

func fetch(ctx context.Context, url string) (*http.Response, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("release URL must use HTTPS")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("release download: %s", resp.Status)
	}
	return resp, nil
}

// Stage downloads m's binary for this platform beside binary, on the same
// filesystem for an atomic rename, and verifies its checksum.
func Stage(ctx context.Context, m Manifest, binary string) (string, error) {
	asset, ok := m.Assets[runtime.GOOS+"-"+runtime.GOARCH]
	if !ok {
		return "", fmt.Errorf("release has no binary for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	r, err := fetch(ctx, asset.URL)
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	f, err := os.CreateTemp(filepath.Dir(binary), ".repogo-update-*")
	if err != nil {
		return "", err
	}
	defer f.Close()
	keep := false
	defer func() {
		if !keep {
			os.Remove(f.Name())
		}
	}()
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, hash), r.Body); err != nil {
		return "", err
	}
	if hex.EncodeToString(hash.Sum(nil)) != asset.SHA256 {
		return "", fmt.Errorf("release checksum mismatch")
	}
	if err := f.Chmod(0o755); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	keep = true
	return f.Name(), nil
}

// Newer reports whether candidate is a later published version than current.
// Published host versions are stable three-part versions; anything else,
// such as a prerelease, is never newer.
func Newer(candidate, current string) bool {
	next, ok := parseVersion(candidate)
	if !ok {
		return false
	}
	if current == "dev" {
		return true
	}
	old, ok := parseVersion(current)
	return ok && slices.Compare(next, old) > 0
}

func parseVersion(v string) ([]int, bool) {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return nil, false
	}
	out := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}
