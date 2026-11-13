package pushgate

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var ourCert = sha256.Sum256([]byte("repogo app signing key"))

// fakePlay answers the OAuth token and decodeIntegrityToken, with verdict for
// whichever token it's sent, counting decodes.
func fakePlay(t *testing.T, verdict func(token string) map[string]any) (*playChecker, *atomic.Int32) {
	t.Helper()
	var decodes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_, _ = w.Write([]byte(`{"access_token":"access-1","expires_in":3600}`))
		case "/decode":
			decodes.Add(1)
			var body struct {
				Token string `json:"integrity_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(w).Encode(map[string]any{"tokenPayloadExternal": verdict(body.Token)})
		}
	}))
	t.Cleanup(server.Close)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &playChecker{
		auth: &googleAuth{email: "relay@example.iam.gserviceaccount.com", key: key, scope: playScope,
			tokenURL: server.URL + "/token", client: server.Client()},
		pkg: "app.repogo", cert: ourCert[:], endpoint: server.URL + "/decode", now: time.Now,
		verdicts: openVerdicts(filepath.Join(t.TempDir(), "verdicts.json"), log),
	}, &decodes
}

// genuine is Play's verdict on the real app, on a real device, for hash at issued.
func genuine(hash string, issued time.Time) map[string]any {
	return map[string]any{
		"requestDetails": map[string]any{"requestPackageName": "app.repogo", "requestHash": hash,
			"timestampMillis": strconv.FormatInt(issued.UnixMilli(), 10)},
		"appIntegrity": map[string]any{"appRecognitionVerdict": "PLAY_RECOGNIZED", "packageName": "app.repogo",
			"certificateSha256Digest": []string{base64.RawURLEncoding.EncodeToString(ourCert[:])}},
		"deviceIntegrity": map[string]any{"deviceRecognitionVerdict": []string{"MEETS_DEVICE_INTEGRITY"}},
	}
}

func always() bool { return true }

func TestPlayIntegrityPassesTheRealAppOnceAGrant(t *testing.T) {
	pub, msg := []byte("phone key"), []byte("grant")
	granted := time.Now().Truncate(time.Millisecond)
	p, decodes := fakePlay(t, func(string) map[string]any { return genuine(IntegrityHash(pub, msg), granted) })
	for range 3 {
		if err := p.check(t.Context(), pub, msg, granted, "tok", always); err != nil {
			t.Fatal(err)
		}
	}
	if decodes.Load() != 1 {
		t.Fatalf("decoded %d times; Google clears a token decoded twice", decodes.Load())
	}
	// A restart keeps the verdict, as the token can't be decoded again.
	again := &playChecker{verdicts: openVerdicts(p.verdicts.path, p.verdicts.log), now: time.Now}
	if err := again.check(t.Context(), pub, msg, granted, "", always); err != nil {
		t.Fatalf("after a restart: %v", err)
	}
}

func TestPlayIntegrityRefusals(t *testing.T) {
	pub, msg := []byte("phone key"), []byte("grant")
	granted := time.Now()
	cases := map[string]func(v map[string]any){
		"another grant": func(v map[string]any) {
			v["requestDetails"].(map[string]any)["requestHash"] = IntegrityHash([]byte("other key"), msg)
		},
		"another app": func(v map[string]any) { v["requestDetails"].(map[string]any)["requestPackageName"] = "com.evil" },
		"stale": func(v map[string]any) {
			v["requestDetails"].(map[string]any)["timestampMillis"] = strconv.FormatInt(granted.Add(-time.Hour).UnixMilli(), 10)
		},
		"sideloaded": func(v map[string]any) {
			v["appIntegrity"].(map[string]any)["appRecognitionVerdict"] = "UNRECOGNIZED_VERSION"
		},
		"another key": func(v map[string]any) {
			v["appIntegrity"].(map[string]any)["certificateSha256Digest"] = []string{"AAAA"}
		},
		"rooted or emulated": func(v map[string]any) { v["deviceIntegrity"] = map[string]any{} },
	}
	for name, spoil := range cases {
		p, _ := fakePlay(t, func(string) map[string]any {
			v := genuine(IntegrityHash(pub, msg), granted)
			spoil(v)
			return v
		})
		if err := p.check(t.Context(), pub, msg, granted, "tok", always); err == nil {
			t.Errorf("%s: passed", name)
		}
	}
	p, decodes := fakePlay(t, func(string) map[string]any { return genuine(IntegrityHash(pub, msg), granted) })
	if err := p.check(t.Context(), pub, msg, granted, "", always); err == nil || decodes.Load() != 0 {
		t.Errorf("no token: %v after %d decodes", err, decodes.Load())
	}
	if err := p.check(t.Context(), pub, msg, granted, "tok", func() bool { return false }); err == nil ||
		!strings.Contains(err.Error(), "too many") || decodes.Load() != 0 {
		t.Errorf("over budget: %v after %d decodes", err, decodes.Load())
	}
}
