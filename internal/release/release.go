// Package release resolves and verifies published host binaries.
package release

import (
	"archive/zip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
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

// publicKey checks manifest.json.sig. The release workflow signs with its
// private half, the RELEASE_SIGNING_KEY secret; npm/repogo.js holds the same key.
var publicKey, _ = base64.StdEncoding.DecodeString("XaWEBib5lq+jel0GkUaC8amZ/87MB7WYL32FEaQBm/Q=")

type Asset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	Version string `json:"version"`
	// Assets are the bare binaries, for Linux.
	Assets map[string]Asset `json:"assets"`
	// Apps are RepoGo Host.app, zipped, for macOS: the bundle gives the host
	// its name and icon in System Settings and privacy prompts.
	Apps map[string]Asset `json:"apps"`
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
	base := "https://github.com/" + Repository + "/releases/download/" + latest.Tag + "/"
	body, err := get(ctx, base+"manifest.json")
	if err != nil {
		return Manifest{}, err
	}
	signature, err := get(ctx, base+"manifest.json.sig")
	if err != nil {
		return Manifest{}, err
	}
	manifest, err := verify(body, signature)
	if err == nil && "v"+manifest.Version != latest.Tag {
		err = fmt.Errorf("release manifest version does not match its tag")
	}
	return manifest, err
}

// verify trusts the manifest's checksums only once its signature matches, so a
// replaced release asset can't name a binary of its own.
func verify(body, signature []byte) (Manifest, error) {
	if !ed25519.Verify(publicKey, body, signature) {
		return Manifest{}, fmt.Errorf("release manifest signature does not match")
	}
	var manifest Manifest
	err := json.Unmarshal(body, &manifest)
	return manifest, err
}

func getJSON(ctx context.Context, url string, into any) error {
	body, err := get(ctx, url)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, into)
}

func get(ctx context.Context, url string) ([]byte, error) {
	r, err := fetch(ctx, url)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, 1<<20))
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

// Root is what an update replaces: on macOS the app bundle binary runs from,
// often through the bin/repogo symlink; elsewhere binary itself.
func Root(binary string) string {
	resolved, err := filepath.EvalSymlinks(binary)
	if err != nil {
		return binary
	}
	macOS := filepath.Dir(resolved)
	app := filepath.Dir(filepath.Dir(macOS))
	if filepath.Base(macOS) == "MacOS" && filepath.Ext(app) == ".app" {
		return app
	}
	return binary
}

// Stage downloads m's release for this platform beside root, on the same
// filesystem for an atomic rename, and verifies its checksum. An app bundle
// root gets the unzipped bundle, any other root the bare binary.
func Stage(ctx context.Context, m Manifest, root string) (string, error) {
	platform := runtime.GOOS + "-" + runtime.GOARCH
	if filepath.Ext(root) != ".app" {
		asset, ok := m.Assets[platform]
		if !ok {
			return "", fmt.Errorf("release has no binary for %s/%s", runtime.GOOS, runtime.GOARCH)
		}
		return download(ctx, asset, filepath.Dir(root))
	}
	asset, ok := m.Apps[platform]
	if !ok {
		return "", fmt.Errorf("release has no app for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	zipped, err := download(ctx, asset, filepath.Dir(root))
	if err != nil {
		return "", err
	}
	defer os.Remove(zipped)
	staged, err := os.MkdirTemp(filepath.Dir(root), ".repogo-update-*")
	if err != nil {
		return "", err
	}
	if err := unzipApp(zipped, staged); err != nil {
		os.RemoveAll(staged)
		return "", err
	}
	return staged, nil
}

// download saves asset into dir, executable, once its checksum matches.
func download(ctx context.Context, asset Asset, dir string) (string, error) {
	r, err := fetch(ctx, asset.URL)
	if err != nil {
		return "", err
	}
	defer r.Body.Close()
	f, err := os.CreateTemp(dir, ".repogo-update-*")
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

// unzipApp writes the bundle zipped at path into dir, without the zip's
// top-level RepoGo Host.app folder. Only folders and plain files are taken.
func unzipApp(path, dir string) error {
	r, err := zip.OpenReader(path)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		top, name, _ := strings.Cut(f.Name, "/")
		if filepath.Ext(top) != ".app" {
			return fmt.Errorf("release app has %q outside its bundle", f.Name)
		}
		if name == "" {
			continue
		}
		if !filepath.IsLocal(name) {
			return fmt.Errorf("release app has an unsafe path %q", f.Name)
		}
		target := filepath.Join(dir, name)
		mode := f.Mode()
		switch {
		case mode.IsDir():
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case mode.IsRegular():
			if err := unzipFile(f, target, mode.Perm()); err != nil {
				return err
			}
		default:
			return fmt.Errorf("release app has %q, which is not a file", f.Name)
		}
	}
	return nil
}

func unzipFile(f *zip.File, target string, perm fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	in, err := f.Open()
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
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
