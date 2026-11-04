package device

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

// SignedPin is the host's TLS key as phones pin it: the SHA-256 of its public
// key (SPKI), vouched for by the identity key every phone already pins. It is
// how trust moves from pairing to TLS with no QR scan.
type SignedPin struct {
	SPKI []byte `json:"spki_sha256"`
	// Counter rises with every new TLS key; a phone keeps the highest it has
	// seen, so a replayed older pin can't bring back a retired key.
	Counter   int64  `json:"counter"`
	Signature []byte `json:"signature"`
}

const (
	pinTag  = "repogo-tls-pin-v1"
	bindTag = "repogo-tls-bind-v1"

	// TLSExporterLabel and TLSExporterLen name the TLS exporter value a hello
	// signs over a passthrough connection (RFC 5705 / 8446 §7.5).
	TLSExporterLabel = "EXPORTER-repogo-hello"
	TLSExporterLen   = 32
)

var ErrBadPin = errors.New("device: TLS pin is not signed by this host")

// PinMessage is what a host signs to vouch for a TLS key.
func PinMessage(host ID, spki []byte, counter int64) ([]byte, error) {
	id, err := host.Bytes()
	if err != nil {
		return nil, err
	}
	if len(spki) != sha256.Size || counter < 1 {
		return nil, ErrBadPin
	}
	msg := make([]byte, 0, len(pinTag)+IDLen+sha256.Size+8)
	msg = append(msg, pinTag...)
	msg = append(msg, id...)
	msg = append(msg, spki...)
	return binary.BigEndian.AppendUint64(msg, uint64(counter)), nil
}

// SignPin vouches for the TLS key whose SPKI hashes to spki.
func (i *Identity) SignPin(spki [sha256.Size]byte, counter int64) (SignedPin, error) {
	msg, err := PinMessage(i.ID, spki[:], counter)
	if err != nil {
		return SignedPin{}, err
	}
	return SignedPin{SPKI: spki[:], Counter: counter, Signature: i.Sign(msg)}, nil
}

// VerifyPin checks a pin against the host key pinned at pairing.
func VerifyPin(host ed25519.PublicKey, p SignedPin) error {
	if len(host) != ed25519.PublicKeySize {
		return ErrBadPin
	}
	msg, err := PinMessage(IDFor(host), p.SPKI, p.Counter)
	if err != nil {
		return err
	}
	if !ed25519.Verify(host, msg, p.Signature) {
		return ErrBadPin
	}
	return nil
}

// BindTLS extends a challenge message with the connection's TLS exporter, so
// a hello signed on one TLS session can't be relayed into another.
func BindTLS(challenge, exporter []byte) []byte {
	msg := make([]byte, 0, len(challenge)+len(bindTag)+len(exporter))
	msg = append(msg, challenge...)
	msg = append(msg, bindTag...)
	return append(msg, exporter...)
}
