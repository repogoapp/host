package device

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"math/big"
)

// A phone key is Ed25519 (32 bytes) or a Secure Enclave P-256 point (65
// bytes, signing r ‖ s over SHA-256), told apart by length (plan 520). The
// kind doesn't prove hardware, so the host never requires P-256.
const (
	P256PublicKeySize = 65
	P256SignatureSize = 64
)

// ValidPhoneKey reports whether pub is a key VerifyPhone can check.
func ValidPhoneKey(pub []byte) bool {
	switch len(pub) {
	case ed25519.PublicKeySize:
		return true
	case P256PublicKeySize:
		_, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pub)
		return err == nil
	}
	return false
}

// VerifyPhone checks sig over msg with a phone's identity key of either kind.
// The host's own key stays Ed25519; this is for keys a phone presents.
func VerifyPhone(pub, msg, sig []byte) bool {
	switch len(pub) {
	case ed25519.PublicKeySize:
		return ed25519.Verify(ed25519.PublicKey(pub), msg, sig)
	case P256PublicKeySize:
		key, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), pub)
		if err != nil || len(sig) != P256SignatureSize {
			return false
		}
		digest := sha256.Sum256(msg)
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		return ecdsa.Verify(key, digest[:], r, s)
	}
	return false
}
