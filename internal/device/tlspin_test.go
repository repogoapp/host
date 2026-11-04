package device

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

func TestPinVerifiesOnlyUnchanged(t *testing.T) {
	host, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	other, _ := Generate()
	pin, err := host.SignPin(sha256.Sum256([]byte("spki")), 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyPin(host.Public, pin); err != nil {
		t.Fatalf("a good pin failed: %v", err)
	}
	if VerifyPin(other.Public, pin) == nil {
		t.Fatal("another host's key verified the pin")
	}
	lower := pin
	lower.Counter = 2
	if VerifyPin(host.Public, lower) == nil {
		t.Fatal("a changed counter verified")
	}
	swapped := pin
	swapped.SPKI = bytes.Repeat([]byte{7}, sha256.Size)
	if VerifyPin(host.Public, swapped) == nil {
		t.Fatal("a swapped key verified")
	}
}

func TestBindTLSChangesTheSignedBytes(t *testing.T) {
	msg := []byte("challenge")
	a, b := BindTLS(msg, []byte{1}), BindTLS(msg, []byte{2})
	if bytes.Equal(a, msg) || bytes.Equal(a, b) {
		t.Fatal("binding did not depend on the exporter")
	}
}

func TestInviteCarriesThePin(t *testing.T) {
	p, _ := openPairer(t)
	host := p.store.Identity()
	pin, _ := host.SignPin(sha256.Sum256([]byte("spki")), 1)
	p.SetTLSPin(pin)
	invite, err := p.Begin("wss://relay.test/ws")
	if err != nil {
		t.Fatal(err)
	}
	if invite.TLSPin == nil || VerifyPin(invite.InviterPublic, *invite.TLSPin) != nil {
		t.Fatalf("invite pin = %+v", invite.TLSPin)
	}
}
