package relay

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"testing"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/handshake"
)

// A phone with a Secure Enclave key (plan 520) says hello with a 65-byte P-256
// key; the relay takes it when its id is the key's fingerprint and it signs.
func TestHelloTakesAP256PhoneKey(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := key.PublicKey.Bytes()
	signed := []byte("challenge message")
	digest := sha256.Sum256(signed)
	r, s, _ := ecdsa.Sign(rand.Reader, key, digest[:])
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	hello := handshake.Hello{
		DeviceID: string(device.IDFor(public)), PublicKey: public, GroupID: "g",
		Role: handshake.RoleClient, ChallengeSig: sig,
	}
	if err := check(hello, signed); err != nil {
		t.Fatalf("a P-256 hello was refused: %v", err)
	}
	hello.ChallengeSig = append([]byte(nil), sig...)
	hello.ChallengeSig[0] ^= 1
	if check(hello, signed) == nil {
		t.Fatal("a P-256 hello with a bad signature was taken")
	}
}
