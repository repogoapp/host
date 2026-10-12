package release

import (
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
