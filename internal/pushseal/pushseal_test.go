package pushseal

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/vectors.json")

func secret(b byte) []byte { return bytes.Repeat([]byte{b}, SecretSize) }

func TestRoundTrip(t *testing.T) {
	key := secret(7)
	env, err := Seal(key, "host1", AgentsState, "", map[string]int{"working": 2})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Open(key, "host1", AgentsState, "", env)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"working":2}` {
		t.Fatalf("opened %s", got)
	}
	if env.V != Version || env.KID != KeyID(key) {
		t.Fatalf("envelope %+v", env)
	}
}

func TestOpenRefuses(t *testing.T) {
	key := secret(7)
	env, _ := Seal(key, "host1", StepsState, "vercel/deploy:web", map[string]string{"title": "Deploy"})
	flipped := env
	box, _ := base64.RawURLEncoding.DecodeString(env.Box)
	box[len(box)-1] ^= 1
	flipped.Box = base64.RawURLEncoding.EncodeToString(box)
	cases := map[string]struct {
		key                 []byte
		host, purpose, bind string
		env                 Envelope
	}{
		"flipped bit":   {key, "host1", StepsState, "vercel/deploy:web", flipped},
		"other host":    {key, "host2", StepsState, "vercel/deploy:web", env},
		"other purpose": {key, "host1", AgentsState, "vercel/deploy:web", env},
		"other bind":    {key, "host1", StepsState, "vercel/deploy:api", env},
		"other key":     {secret(8), "host1", StepsState, "vercel/deploy:web", env},
		"other version": {key, "host1", StepsState, "vercel/deploy:web", Envelope{V: 2, KID: env.KID, Box: env.Box}},
		"short box":     {key, "host1", StepsState, "vercel/deploy:web", Envelope{V: 1, KID: env.KID, Box: "AAAA"}},
	}
	for name, c := range cases {
		if _, err := Open(c.key, c.host, c.purpose, c.bind, c.env); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
}

// The box's length moves only in steps of the padding.
func TestPadding(t *testing.T) {
	key := secret(1)
	size := func(body string) int {
		env, _ := sealBytes(key, "h", FCM, "", []byte(body), zeroRandom)
		return len(env.Box)
	}
	if size(`{"working":1}`) != size(`{"working":10}`) {
		t.Error("a digit more changed the size")
	}
	for n := range 100 {
		box, _ := base64.RawURLEncoding.DecodeString(func() string {
			env, _ := sealBytes(key, "h", FCM, "", bytes.Repeat([]byte("a"), n), zeroRandom)
			return env.Box
		}())
		const tagSize = 16 // GCM's
		if plain := len(box) - nonceSize - tagSize; plain%padTo != 0 {
			t.Fatalf("body %d: plaintext %d not padded", n, plain)
		}
	}
}

func TestSecretSize(t *testing.T) {
	if _, err := Seal(make([]byte, 16), "h", FCM, "", 1); err == nil {
		t.Fatal("sealed with a short secret")
	}
}

// vector is one case the phones' tests check their PushSeal against.
type vector struct {
	Secret  string   `json:"secret"`
	KID     string   `json:"kid"`
	Nonce   string   `json:"nonce"`
	Host    string   `json:"host"`
	Purpose string   `json:"purpose"`
	Bind    string   `json:"bind"`
	Body    string   `json:"body"`
	Sealed  Envelope `json:"sealed"`
}

func zeroRandom(b []byte) (int, error) { clear(b); return len(b), nil }

func fixedRandom(nonce []byte) func([]byte) (int, error) {
	return func(b []byte) (int, error) { return copy(b, nonce), nil }
}

// TestVectors pins Seal's exact output, so Swift and Kotlin can match it.
func TestVectors(t *testing.T) {
	cases := []struct {
		secret              []byte
		nonce               []byte
		host, purpose, bind string
		body                string
	}{
		{secret(0x11), bytes.Repeat([]byte{0x01}, nonceSize), "Hk3vA9", AgentsState, "", `{"working":2,"needs_you":1,"finished":0,"at":1764000123000}`},
		{secret(0x22), bytes.Repeat([]byte{0x02}, nonceSize), "Hk3vA9", StepsAttrs, "", `{"key":"vercel/deploy:web","project":"/Users/me/web"}`},
		{secret(0x22), bytes.Repeat([]byte{0x03}, nonceSize), "Hk3vA9", StepsState, "vercel/deploy:web", `{"title":"Deploying","body":"Building … 3/5 ✓","at":1764000124000}`},
		{secret(0x33), bytes.Repeat([]byte{0x04}, nonceSize), "Hk3vA9", FCM, "", `{"event":"update","kind":"agents"}`},
		{secret(0x44), bytes.Repeat([]byte{0x05}, nonceSize), "Hk3vA9", Alert, "", `{"title":"Fix the login bug","subtitle":"Claude finished in web","body":"Fixed the token refresh; tests pass.","thread":"claude:s1","fields":{"chat_id":"claude:s1","kind":"turn_end"}}`},
	}
	var got []vector
	for _, c := range cases {
		env, err := sealBytes(c.secret, c.host, c.purpose, c.bind, []byte(c.body), fixedRandom(c.nonce))
		if err != nil {
			t.Fatal(err)
		}
		opened, err := Open(c.secret, c.host, c.purpose, c.bind, env)
		if err != nil || string(opened) != c.body {
			t.Fatalf("%s: does not open: %v", c.purpose, err)
		}
		got = append(got, vector{
			Secret: hex.EncodeToString(c.secret), KID: KeyID(c.secret), Nonce: hex.EncodeToString(c.nonce),
			Host: c.host, Purpose: c.purpose, Bind: c.bind, Body: c.body, Sealed: env,
		})
	}
	data, _ := json.MarshalIndent(got, "", "  ")
	data = append(data, '\n')
	path := filepath.Join("testdata", "vectors.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; run: go test ./internal/pushseal -update", err)
	}
	if !bytes.Equal(want, data) {
		t.Fatal("vectors changed; run: go test ./internal/pushseal -update, and update the phones if the format moved")
	}
}
