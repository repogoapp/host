package device

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

func TestNewCodeShape(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		code := NewCode()
		if len(code) != CodeLen || strings.Trim(code, codeAlphabet) != "" {
			t.Fatalf("code %q", code)
		}
		seen[code] = true
	}
	if len(seen) != 100 {
		t.Fatal("codes repeated")
	}
}

// Known answer shared with PairingCodeTests.swift.
func TestCodeKnownAnswer(t *testing.T) {
	const code, payload = "K7QX2M9PLB4ZTR", "eyJhIjoxfQ"
	if got := CodeLookup(code); got != "e766fc38a5bf5ebcd00dd3295ca3f0b0" {
		t.Fatalf("lookup = %s", got)
	}
	sealed, err := SealInvite(code, payload)
	if err != nil {
		t.Fatal(err)
	}
	if sealed != "eUTPB5mhimlCGUz05moOCG6MSM7eZItewG0" {
		t.Fatalf("sealed = %s", sealed)
	}
	key := sha256.Sum256([]byte(sealTag + code))
	aead, _ := chacha20poly1305.New(key[:])
	raw, _ := base64.RawURLEncoding.DecodeString(sealed)
	plain, err := aead.Open(nil, make([]byte, 12), raw, nil)
	if err != nil || string(plain) != payload {
		t.Fatalf("open: %v %q", err, plain)
	}
	if CodeLookup("k7qx2m9plb4ztr") != CodeLookup(code) {
		t.Fatal("lookup is case sensitive")
	}
}
