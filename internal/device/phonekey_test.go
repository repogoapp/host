package device

import (
	"bytes"
	"testing"
)

func TestBothPhoneKeyKindsVerify(t *testing.T) {
	msg := []byte("challenge")
	ed, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !ValidPhoneKey(ed.Public) || !VerifyPhone(ed.Public, msg, ed.Sign(msg)) {
		t.Fatal("an Ed25519 phone key did not verify")
	}
	se := newP256Phone(t)
	sig := se.sign(t, msg)
	if !ValidPhoneKey(se.public) || !VerifyPhone(se.public, msg, sig) {
		t.Fatal("a P-256 phone key did not verify")
	}
	if VerifyPhone(se.public, []byte("other"), sig) {
		t.Fatal("a P-256 signature verified another message")
	}
	if VerifyPhone(se.public, msg, sig[:63]) {
		t.Fatal("a short P-256 signature verified")
	}
	// A 65-byte value that isn't a point on the curve is no key.
	if ValidPhoneKey(bytes.Repeat([]byte{4}, P256PublicKeySize)) {
		t.Fatal("a point off the curve was taken as a key")
	}
	if ValidPhoneKey(make([]byte, 48)) {
		t.Fatal("a key of neither size was taken")
	}
}
