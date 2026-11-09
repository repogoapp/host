package binfetch

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const script = "#!/bin/sh\necho fetched\n"

func tarGz(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct{ name, body string }{{"pkg/LICENSE", "MIT"}, {name, body}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(f.body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func zipped(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte(body))
	zw.Close()
	return buf.Bytes()
}

// serve is a fake github.com: a /releases/latest redirect and two archives.
func serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			http.Redirect(w, r, "/cli/cli/releases/tag/v2.101.0", http.StatusFound)
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			w.Write(tarGz(t, "gh_2.101.0_linux_arm64/bin/gh", script))
		case strings.HasSuffix(r.URL.Path, ".zip"):
			w.Write(zipped(t, "gh_2.101.0_macOS_arm64/bin/gh", script))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := Base
	Base = srv.URL
	t.Cleanup(func() { Base = old })
	t.Setenv("HOME", t.TempDir())
}

func TestLatestReadsTheRedirect(t *testing.T) {
	serve(t)
	tag, err := Latest(context.Background(), "cli/cli")
	if err != nil || tag != "v2.101.0" {
		t.Fatalf("tag %q, err %v", tag, err)
	}
}

func TestInstallTakesTheMemberFromEitherArchive(t *testing.T) {
	serve(t)
	for _, ext := range []string{".tar.gz", ".zip"} {
		path, err := Install(context.Background(), Base+"/gh"+ext, "bin/gh", "gh")
		if err != nil {
			t.Fatalf("%s: %v", ext, err)
		}
		dir, _ := Dir()
		st, err := os.Stat(path)
		if err != nil || path != filepath.Join(dir, "gh") || st.Mode()&0o111 == 0 {
			t.Fatalf("%s: %s %v %v", ext, path, st, err)
		}
		if b, _ := os.ReadFile(path); string(b) != script {
			t.Fatalf("%s: wrote %q", ext, b)
		}
		os.Remove(path)
	}
}

func TestInstallRefusesAMissingMemberAndLeavesNothing(t *testing.T) {
	serve(t)
	if _, err := Install(context.Background(), Base+"/x.tar.gz", "bin/codex", "codex"); err == nil {
		t.Fatal("installed a member the archive does not have")
	}
	if _, err := Install(context.Background(), Base+"/missing", "bin/gh", "gh"); err == nil {
		t.Fatal("installed from a 404")
	}
	dir, _ := Dir()
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("left %v behind", entries)
	}
}

func TestOnPathPrependsOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", "/usr/bin")
	OnPath()
	OnPath()
	dir, _ := Dir()
	if got := os.Getenv("PATH"); got != dir+string(os.PathListSeparator)+"/usr/bin" {
		t.Fatalf("PATH = %q", got)
	}
}

func tree(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, h := range headers {
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(h.Name))
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write([]byte(h.Name))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestInstallTreeUnpacksWithoutTheTopFolder(t *testing.T) {
	archive := tree(t,
		&tar.Header{Name: "pkg/bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "pkg/bin/tool", Typeflag: tar.TypeReg, Mode: 0o755},
		&tar.Header{Name: "pkg/bin/alias", Typeflag: tar.TypeSymlink, Linkname: "tool"},
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(archive) }))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "pkg")
	os.MkdirAll(filepath.Join(dest, "stale"), 0o755)

	if err := InstallTree(context.Background(), srv.URL+"/pkg.tar.gz", sum(archive), dest); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "bin", "alias")); string(b) != "pkg/bin/tool" {
		t.Fatalf("alias reads %q", b)
	}
	if _, err := os.Stat(filepath.Join(dest, "stale")); err == nil {
		t.Fatal("the old tree survived")
	}
}

func TestInstallTreeRefusesEscapes(t *testing.T) {
	for name, h := range map[string]*tar.Header{
		"dotdot":        {Name: "pkg/../../evil", Typeflag: tar.TypeReg, Mode: 0o644},
		"link out":      {Name: "pkg/out", Typeflag: tar.TypeSymlink, Linkname: "../../evil"},
		"absolute link": {Name: "pkg/abs", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
	} {
		archive := tree(t, h)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(archive) }))
		dest := filepath.Join(t.TempDir(), "pkg")
		if err := InstallTree(context.Background(), srv.URL+"/x.tar.gz", sum(archive), dest); err == nil {
			t.Errorf("%s: unpacked", name)
		}
		if _, err := os.Stat(dest); err == nil {
			t.Errorf("%s: left %s behind", name, dest)
		}
		srv.Close()
	}
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestUntarRejectsComposedSymlinkEscape(t *testing.T) {
	for _, withFile := range []bool{false, true} {
		dir := t.TempDir()
		dest := filepath.Join(dir, "unpacked")
		outside := filepath.Join(dir, "outside")
		if err := os.Mkdir(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(outside, 0o755); err != nil {
			t.Fatal(err)
		}
		victim := filepath.Join(outside, "file")
		if err := os.WriteFile(victim, []byte("untouched"), 0o644); err != nil {
			t.Fatal(err)
		}
		headers := []*tar.Header{
			{Name: "pkg/a", Typeflag: tar.TypeSymlink, Linkname: "."},
			{Name: "pkg/b", Typeflag: tar.TypeSymlink, Linkname: "a/../outside"},
		}
		if withFile {
			headers = append(headers, &tar.Header{Name: "pkg/b/file", Typeflag: tar.TypeReg, Mode: 0o644})
		}
		if err := Untar(tree(t, headers...), dest); err == nil {
			t.Fatalf("withFile=%v: accepted escape", withFile)
		}
		if got, err := os.ReadFile(victim); err != nil || string(got) != "untouched" {
			t.Fatalf("outside file: %q, %v", got, err)
		}
	}
}

// A download that is not the archive its checksum names is never unpacked.
func TestInstallTreeRefusesAChecksumMismatch(t *testing.T) {
	archive := tree(t, &tar.Header{Name: "pkg/tool", Typeflag: tar.TypeReg, Mode: 0o755})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(archive) }))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "pkg")
	if err := InstallTree(context.Background(), srv.URL+"/pkg.tar.gz", sum([]byte("other")), dest); err == nil {
		t.Fatal("installed an archive that does not match its checksum")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Fatal("left the tree behind")
	}
}

func TestInstallPackageKeepsTheRootLayout(t *testing.T) {
	archive := tree(t,
		&tar.Header{Name: "bin/", Typeflag: tar.TypeDir, Mode: 0o755},
		&tar.Header{Name: "bin/codex", Typeflag: tar.TypeReg, Mode: 0o755},
		&tar.Header{Name: "bin/codex-code-mode-host", Typeflag: tar.TypeReg, Mode: 0o755},
		&tar.Header{Name: "codex-resources/bwrap", Typeflag: tar.TypeReg, Mode: 0o755},
		&tar.Header{Name: "codex-package.json", Typeflag: tar.TypeReg, Mode: 0o644},
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(archive) }))
	defer srv.Close()
	dest := filepath.Join(t.TempDir(), "rust-v1")

	if err := InstallPackage(context.Background(), srv.URL+"/p.tar.gz", sum(archive), dest); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"bin/codex", "bin/codex-code-mode-host", "codex-resources/bwrap", "codex-package.json"} {
		if b, _ := os.ReadFile(filepath.Join(dest, f)); string(b) != f {
			t.Fatalf("%s reads %q", f, b)
		}
	}
}

func TestDigestReadsTheAssetsSHA256(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/openai/codex/releases/tags/rust-v1" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"assets":[{"name":"other.tar.gz","digest":"sha256:00"},{"name":"pkg.tar.gz","digest":"sha256:abc123"},{"name":"bare.tar.gz"}]}`))
	}))
	defer srv.Close()
	old := API
	API = srv.URL
	t.Cleanup(func() { API = old })
	ctx := context.Background()

	if got, err := Digest(ctx, "openai/codex", "rust-v1", "pkg.tar.gz"); err != nil || got != "abc123" {
		t.Fatalf("digest %q, %v", got, err)
	}
	if _, err := Digest(ctx, "openai/codex", "rust-v1", "bare.tar.gz"); err == nil {
		t.Fatal("an asset with no digest was accepted")
	}
	if _, err := Digest(ctx, "openai/codex", "rust-v1", "missing.tar.gz"); err == nil {
		t.Fatal("a missing asset was accepted")
	}
	if _, err := Digest(ctx, "openai/codex", "rust-v2", "pkg.tar.gz"); err == nil {
		t.Fatal("a missing release was accepted")
	}
}
