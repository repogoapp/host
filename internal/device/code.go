package device

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"

	"golang.org/x/crypto/chacha20poly1305"
)

// The pairing code is 14 base36 characters, what an App Clip Code can hold. The
// pair host keeps the invite under the code's hash, sealed with a key derived from
// it, so it can neither read nor swap it. PairingClient.swift derives both alike.
const (
	CodeLen      = 14
	codeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	lookupTag    = "repogo-pair-lookup:"
	sealTag      = "repogo-pair-key:"
)

func NewCode() string {
	b := make([]byte, CodeLen)
	rand.Read(b)
	// 256 mod 36 is not zero; rejection keeps every character uniform.
	for i := range b {
		for b[i] >= 252 {
			rand.Read(b[i : i+1])
		}
		b[i] = codeAlphabet[b[i]%36]
	}
	return string(b)
}

// CodeLookup is what the pair host indexes by: 128 bits of the code's hash.
func CodeLookup(code string) string {
	sum := sha256.Sum256([]byte(lookupTag + strings.ToUpper(code)))
	return hex.EncodeToString(sum[:16])
}

// SealInvite encrypts an encoded invite under the code. The key is single use,
// so the nonce is zero.
func SealInvite(code, payload string) (string, error) {
	key := sha256.Sum256([]byte(sealTag + strings.ToUpper(code)))
	aead, err := chacha20poly1305.New(key[:])
	if err != nil {
		return "", err
	}
	sealed := aead.Seal(nil, make([]byte, chacha20poly1305.NonceSize), []byte(payload), nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}
