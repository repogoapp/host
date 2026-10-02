package nodejs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/binfetch"
)

// release is a Node archive whose node is a script answering -v.
func release(t *testing.T, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	top := "node-" + version + "/"
	node := "#!/bin/sh\necho " + version + "\n"
	for _, h := range []*tar.Header{
		{Name: top + "bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: top + "bin/node", Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(node))},
		{Name: top + "bin/npx", Typeflag: tar.TypeSymlink, Linkname: "../lib/npx-cli.js"},
		{Name: top + "lib/npx-cli.js", Typeflag: tar.TypeReg, Mode: 0o755},
	} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Name == top+"bin/node" {
			tw.Write([]byte(node))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// nodeOrg is what the fake nodejs.org serves.
type nodeOrg struct {
	fetches  int
	archive  []byte // served for every node-v24.21.0 asset
	summed   []byte // what SHASUMS256.txt lists for them
	indexErr bool
}

// serve is a fake nodejs.org, and a PATH with no node on it.
func serve(t *testing.T) *nodeOrg {
	t.Helper()
	archive := release(t, "v24.21.0")
	org := &nodeOrg{archive: archive, summed: archive}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/index.json" && org.indexErr:
			http.Error(w, "down", http.StatusServiceUnavailable)
		case r.URL.Path == "/index.json":
			w.Write([]byte(`[{"version":"v25.0.0","lts":false},{"version":"v24.21.0","lts":"Krypton"}]`))
		case r.URL.Path == "/v24.21.0/SHASUMS256.txt":
			sum := sha256.Sum256(org.summed)
			for _, sys := range []string{"darwin-arm64", "darwin-x64", "linux-arm64", "linux-x64"} {
				fmt.Fprintf(w, "%x  node-v24.21.0-%s.tar.gz\n", sum, sys)
			}
		case strings.HasPrefix(r.URL.Path, "/v24.21.0/node-v24.21.0-") && strings.HasSuffix(r.URL.Path, ".tar.gz"):
			org.fetches++
			w.Write(org.archive)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := Dist
	Dist = srv.URL
	t.Cleanup(func() { Dist = old })
	t.Setenv(apphome.EnvVar, t.TempDir())
	t.Setenv("PATH", t.TempDir()+string(os.PathListSeparator)+"/bin")
	return org
}

func TestEnsureInstallsTheLTSOnce(t *testing.T) {
	if _, err := asset("v1", runtime.GOOS, runtime.GOARCH); err != nil {
		t.Skip(err)
	}
	org := serve(t)
	bin, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := apphome.Path("node", "bin")
	if bin != dir {
		t.Fatalf("bin = %q, want %q", bin, dir)
	}
	if link, err := os.Readlink(filepath.Join(bin, "npx")); err != nil || link != "../lib/npx-cli.js" {
		t.Fatalf("npx -> %q, %v", link, err)
	}
	// The host's own copy serves the next start without a download.
	if again, err := Ensure(context.Background()); err != nil || again != bin || org.fetches != 1 {
		t.Fatalf("second Ensure = %q, %v after %d fetches", again, err, org.fetches)
	}
}

func TestEnsureKeepsTheMachinesNewEnoughNode(t *testing.T) {
	org := serve(t)
	path := strings.Split(os.Getenv("PATH"), string(os.PathListSeparator))[0]
	os.WriteFile(filepath.Join(path, "node"), []byte("#!/bin/sh\necho v22.3.0\n"), 0o755)
	if bin, err := Ensure(context.Background()); err != nil || bin != "" || org.fetches != 0 {
		t.Fatalf("Ensure = %q, %v after %d fetches", bin, err, org.fetches)
	}
	// Too old counts as none.
	os.WriteFile(filepath.Join(path, "node"), []byte("#!/bin/sh\necho v18.19.0\n"), 0o755)
	if _, err := asset("v1", runtime.GOOS, runtime.GOARCH); err != nil {
		t.Skip(err)
	}
	if bin, err := Ensure(context.Background()); err != nil || bin == "" || org.fetches != 1 {
		t.Fatalf("with node 18: %q, %v after %d fetches", bin, err, org.fetches)
	}
}

// A Node whose archive is not the one nodejs.org's checksums name is refused.
func TestEnsureRefusesATamperedArchive(t *testing.T) {
	if _, err := asset("v1", runtime.GOOS, runtime.GOARCH); err != nil {
		t.Skip(err)
	}
	org := serve(t)
	org.summed = []byte("the real archive")
	if bin, err := Ensure(context.Background()); err == nil {
		t.Fatalf("installed a tampered archive at %q", bin)
	}
	if dir, _ := apphome.Path("node"); fileExists(dir) {
		t.Fatal("the tampered tree was unpacked")
	}
}

// An error page is not a release index.
func TestLatestLTSChecksTheStatus(t *testing.T) {
	org := serve(t)
	org.indexErr = true
	if v, err := latestLTS(context.Background()); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("latestLTS = %q, %v", v, err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestAssetNames(t *testing.T) {
	for sys, want := range map[[2]string]string{
		{"linux", "arm64"}:  "node-v24.21.0-linux-arm64.tar.gz",
		{"linux", "amd64"}:  "node-v24.21.0-linux-x64.tar.gz",
		{"darwin", "arm64"}: "node-v24.21.0-darwin-arm64.tar.gz",
	} {
		if got, err := asset("v24.21.0", sys[0], sys[1]); err != nil || got != want {
			t.Fatalf("%v: %q, %v", sys, got, err)
		}
	}
	if _, err := asset("v24.21.0", "windows", "amd64"); !errors.Is(err, binfetch.ErrUnsupported) {
		t.Fatalf("windows: %v", err)
	}
}
