// Package binfetch installs a CLI from its own GitHub release into
// ~/.local/bin, so a machine with no Homebrew, no npm and no sudo can still
// get one from a phone.
package binfetch

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Base is GitHub; tests point it at a local server.
var Base = "https://github.com"

const (
	// A CLI is tens of megabytes; this bounds a wrong URL, not a real one.
	maxArchive = 512 << 20

	// Generous for a large download on a slow connection.
	fetchTimeout = 5 * time.Minute
)

var ErrUnsupported = errors.New("binfetch: no release build for this system")

// Dir is where binaries land: the directory Claude Code's own installer uses.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".local", "bin"), err
}

// OnPath puts Dir first on this process's PATH, so a binary installed after the
// host started is found by LookPath and by every agent the host spawns.
func OnPath() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	return PrependPath(dir)
}

// PrependPath puts dir first on this process's PATH, once.
func PrependPath(dir string) error {
	path := os.Getenv("PATH")
	for _, p := range filepath.SplitList(path) {
		if p == dir {
			return nil
		}
	}
	return os.Setenv("PATH", dir+string(os.PathListSeparator)+path)
}

// Latest is a repository's newest release tag, read from the redirect GitHub
// serves for /releases/latest; the API would spend a rate-limited call on it.
func Latest(ctx context.Context, repo string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, Base+"/"+repo+"/releases/latest", nil)
	if err != nil {
		return "", err
	}
	client := http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	_, tag, ok := strings.Cut(loc, "/releases/tag/")
	if !ok || tag == "" {
		return "", fmt.Errorf("binfetch: no latest release for %s (%s)", repo, resp.Status)
	}
	return tag, nil
}

// Install downloads a .tar.gz or .zip, takes the one file whose path ends in
// member, and puts it at Dir/name. It returns the installed path.
func Install(ctx context.Context, url, member, name string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	archive, err := Fetch(ctx, url)
	if err != nil {
		return "", err
	}
	var bin []byte
	if strings.HasSuffix(url, ".zip") {
		bin, err = fromZip(archive, member)
	} else {
		bin, err = fromTarGz(archive, member)
	}
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	// Written beside the target and renamed, so a running copy is never
	// half-overwritten and a failed write leaves the old one.
	tmp, err := os.CreateTemp(dir, "."+name+"-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(bin); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, name)
	return dest, os.Rename(tmp.Name(), dest)
}

// InstallTree downloads a .tar.gz whose sha256 is sum (hex) and unpacks all of
// it at dest, without its top-level folder, replacing what was there only once
// the whole tree is out.
func InstallTree(ctx context.Context, url, sum, dest string) error {
	archive, err := Fetch(ctx, url)
	if err != nil {
		return err
	}
	if got := sha256.Sum256(archive); hex.EncodeToString(got[:]) != strings.ToLower(sum) {
		return fmt.Errorf("binfetch: %s does not match its checksum", url)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), "."+filepath.Base(dest)+"-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := Untar(archive, tmp); err != nil {
		return err
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// Untar writes every entry below its first path component into dir. An entry
// or link that would land outside dir is refused rather than cleaned.
func Untar(archive []byte, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var links []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			// Check complete link chains after every target has been extracted.
			for _, name := range links {
				if _, err := root.Stat(name); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("binfetch: unsafe link %s: %w", name, err)
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		_, rel, ok := strings.Cut(strings.TrimPrefix(h.Name, "./"), "/")
		if !ok || rel == "" {
			continue
		}
		// os.Root refuses a trailing slash, which tar gives every directory.
		rel = filepath.Clean(rel)
		if !filepath.IsLocal(rel) {
			return fmt.Errorf("binfetch: %s leaves the archive", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(rel, 0o755)
		case tar.TypeReg:
			err = writeFile(root, rel, tr, os.FileMode(h.Mode)&0o777)
		case tar.TypeSymlink:
			if filepath.IsAbs(h.Linkname) || !filepath.IsLocal(filepath.Join(filepath.Dir(rel), h.Linkname)) {
				return fmt.Errorf("binfetch: %s links outside the archive", h.Name)
			}
			if err = root.MkdirAll(filepath.Dir(rel), 0o755); err == nil {
				err = root.Symlink(h.Linkname, rel)
				links = append(links, rel)
			}
		}
		if err != nil {
			return err
		}
	}
}

func writeFile(root *os.Root, path string, r io.Reader, mode os.FileMode) error {
	if err := root.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := root.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Fetch is the body of a GET that answered 200, bounded in size and time.
func Fetch(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("binfetch: %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxArchive))
}

func fromTarGz(archive []byte, member string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("binfetch: %s is not in the archive", member)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && matches(h.Name, member) {
			return io.ReadAll(tr)
		}
	}
}

func fromZip(archive []byte, member string) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if f.Mode().IsRegular() && matches(f.Name, member) {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("binfetch: %s is not in the archive", member)
}

// matches is the whole name or a path ending in "/"+member.
func matches(name, member string) bool {
	return name == member || strings.HasSuffix(name, "/"+member)
}
