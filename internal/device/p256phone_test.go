package device

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"testing"
)

// p256Phone plays an iPhone whose key is in the Secure Enclave: a P-256 key
// that signs r ‖ s over SHA-256(msg), as CryptoKit's rawRepresentation.
type p256Phone struct {
	key    *ecdsa.PrivateKey
	public []byte
}

func newP256Phone(t *testing.T) p256Phone {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := key.PublicKey.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return p256Phone{key: key, public: public}
}

func (p p256Phone) sign(t *testing.T, msg []byte) []byte {
	t.Helper()
	digest := sha256.Sum256(msg)
	r, s, err := ecdsa.Sign(rand.Reader, p.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
}
