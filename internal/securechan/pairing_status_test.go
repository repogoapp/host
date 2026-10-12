package securechan

import (
	"crypto/ed25519"
	"crypto/sha256"
	"testing"

	"github.com/repogo/host/internal/device"
)

func TestPairingRejectionBindsFreshAttempt(t *testing.T) {
	host, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	phone, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	request := make([]byte, Message1Len)
	request[0] = PairedHandshake
	refusal, err := PairingRejection(host, phone.ID, request)
	if err != nil {
		t.Fatal(err)
	}
	phoneID, _ := phone.ID.Bytes()
	hostID, _ := host.ID.Bytes()
	digest := sha256.Sum256(request)
	message := append([]byte("repogo-not-paired-v1"), phoneID...)
	message = append(message, hostID...)
	message = append(message, digest[:]...)
	if len(refusal) != 65 || refusal[0] != NotPaired || !ed25519.Verify(host.Public, message, refusal[1:]) {
		t.Fatal("refusal did not authenticate the host, phone and attempt")
	}
	message[len(message)-1] ^= 1
	if ed25519.Verify(host.Public, message, refusal[1:]) {
		t.Fatal("refusal replayed for a different attempt")
	}
	for _, invalid := range [][]byte{nil, {PairedHandshake}, append([]byte{Handshake}, request[1:]...)} {
		if _, err := PairingRejection(host, phone.ID, invalid); err == nil {
			t.Fatal("signed a malformed or pairing request")
		}
	}
}
