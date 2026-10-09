package device

import (
	"crypto/ed25519"
	"encoding/json"
)

// PushProof binds a paired identity's grant to an Apple-attested app instance.
type PushProof struct {
	Signature   []byte `json:"signature"`
	Attestation []byte `json:"attestation"`
	Assertion   []byte `json:"assertion"`
}

const MaxPushProofBytes = 32 << 10

func VerifyPushProof(pub, message, raw []byte) (PushProof, error) {
	var p PushProof
	if len(raw) > MaxPushProofBytes || json.Unmarshal(raw, &p) != nil ||
		len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, message, p.Signature) {
		return PushProof{}, ErrBadGrant
	}
	return p, nil
}
