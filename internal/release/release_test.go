package release

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		candidate, current string
		want               bool
	}{
		{"0.2.0", "0.1.9", true},
		{"0.10.0", "0.9.0", true},
		{"1.0.0", "1.0.0", false},
		{"0.1.0", "0.2.0", false},
		{"0.2.0", "dev", true},
		{"0.2", "0.1.0", false},
		{"0.2.0-beta", "0.1.0", false},
		{"0.2.0", "0.1", false},
		{"", "dev", false},
	} {
		if got := Newer(c.candidate, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.candidate, c.current, got, c.want)
		}
	}
}

// serve answers every request with body over HTTPS, as GitHub would.
func serve(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(body) }))
	t.Cleanup(srv.Close)
	old := client
	client = srv.Client()
	t.Cleanup(func() { client = old })
	return srv.URL
}

func manifest(url, sum string) Manifest {
	return Manifest{Version: "0.2.0", Assets: map[string]Asset{runtime.GOOS + "-" + runtime.GOARCH: {URL: url, SHA256: sum}}}
}

func TestStageVerifiesTheChecksum(t *testing.T) {
	body := []byte("the new host")
	url := serve(t, body)
	dir := t.TempDir()
	binary := filepath.Join(dir, "repogo")

	sum := sha256.Sum256(body)
	staged, err := Stage(context.Background(), manifest(url+"/repogo", hex.EncodeToString(sum[:])), binary)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(staged); string(got) != string(body) || filepath.Dir(staged) != dir {
		t.Fatalf("staged %q at %s", got, staged)
	}
	os.Remove(staged)

	other := sha256.Sum256([]byte("something else"))
	if _, err := Stage(context.Background(), manifest(url+"/repogo", hex.EncodeToString(other[:])), binary); err == nil {
		t.Fatal("staged a binary whose checksum does not match")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("a refused download was left behind: %v", entries)
	}
}

func TestStageRefusesPlainHTTP(t *testing.T) {
	if _, err := Stage(context.Background(), manifest("http://example.com/repogo", ""), filepath.Join(t.TempDir(), "repogo")); err == nil {
		t.Fatal("staged over plain HTTP")
	}
}

func TestPublicKeyIsEd25519(t *testing.T) {
	if len(publicKey) != ed25519.PublicKeySize {
		t.Fatalf("public key is %d bytes, want %d", len(publicKey), ed25519.PublicKeySize)
	}
}

func TestVerifyChecksTheSignature(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	old := publicKey
	publicKey = public
	t.Cleanup(func() { publicKey = old })

	body := []byte(`{"version":"0.2.0","assets":{}}`)
	signature := ed25519.Sign(private, body)
	if m, err := verify(body, signature); err != nil || m.Version != "0.2.0" {
		t.Fatalf("verify = %+v, %v", m, err)
	}
	if _, err := verify([]byte(`{"version":"0.2.1","assets":{}}`), signature); err == nil {
		t.Fatal("accepted a changed manifest")
	}
	_, other, _ := ed25519.GenerateKey(nil)
	if _, err := verify(body, ed25519.Sign(other, body)); err == nil {
		t.Fatal("accepted a manifest signed with another key")
	}
	if _, err := verify(body, nil); err == nil {
		t.Fatal("accepted a manifest with no signature")
	}
}

// zipApp zips files under a RepoGo Host.app folder, as ditto --keepParent does.
func zipApp(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for name, body := range files {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0o755)
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(body))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func appManifest(url string, body []byte) Manifest {
	sum := sha256.Sum256(body)
	return Manifest{Version: "0.2.0", Apps: map[string]Asset{runtime.GOOS + "-" + runtime.GOARCH: {URL: url, SHA256: hex.EncodeToString(sum[:])}}}
}

func TestStageUnzipsTheApp(t *testing.T) {
	body := zipApp(t, map[string]string{
		"RepoGo Host.app/Contents/MacOS/repogo": "the new host",
		"RepoGo Host.app/Contents/Info.plist":   "plist",
	})
	url := serve(t, body)
	dir := t.TempDir()
	staged, err := Stage(context.Background(), appManifest(url+"/repogo.app.zip", body), filepath.Join(dir, "RepoGo Host.app"))
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(staged, "Contents/MacOS/repogo")
	if got, _ := os.ReadFile(binary); string(got) != "the new host" || filepath.Dir(staged) != dir {
		t.Fatalf("staged %q at %s", got, staged)
	}
	if info, err := os.Stat(binary); err != nil || info.Mode().Perm()&0o100 == 0 {
		t.Fatalf("staged binary is not executable: %v", info.Mode())
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("the zip was left beside the staged bundle: %v", entries)
	}
}

func TestStageRefusesAnAppReachingOutside(t *testing.T) {
	for _, name := range []string{"RepoGo Host.app/../../evil", "elsewhere/repogo", "__MACOSX/._repogo"} {
		body := zipApp(t, map[string]string{name: "x"})
		url := serve(t, body)
		dir := t.TempDir()
		if _, err := Stage(context.Background(), appManifest(url+"/repogo.app.zip", body), filepath.Join(dir, "RepoGo Host.app")); err == nil {
			t.Errorf("staged an app with %q", name)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("a refused app was left behind: %v", entries)
		}
	}
}

// The service starts bin/repogo, a symlink into the bundle, and the update
// replaces the bundle the symlink names.
func TestRootIsTheAppHoldingTheBinary(t *testing.T) {
	dir := t.TempDir()
	app := filepath.Join(dir, "RepoGo Host.app")
	if err := os.MkdirAll(filepath.Join(app, "Contents/MacOS"), 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(app, "Contents/MacOS/repogo")
	os.WriteFile(inside, nil, 0o755)
	link := filepath.Join(dir, "repogo")
	if err := os.Symlink("RepoGo Host.app/Contents/MacOS/repogo", link); err != nil {
		t.Fatal(err)
	}
	resolvedApp, _ := filepath.EvalSymlinks(app)
	if got := Root(link); got != resolvedApp {
		t.Errorf("Root(symlink) = %s, want %s", got, resolvedApp)
	}
	if got := Root(inside); got != resolvedApp {
		t.Errorf("Root(inside) = %s, want %s", got, resolvedApp)
	}
	bare := filepath.Join(dir, "bare")
	os.WriteFile(bare, nil, 0o755)
	if got := Root(bare); got != bare {
		t.Errorf("Root(bare) = %s, want itself", got)
	}
}
