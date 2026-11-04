package device

import (
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
		!ValidPhoneKey(pub) || !VerifyPhone(pub, message, p.Signature) {
		return PushProof{}, ErrBadGrant
	}
	return p, nil
}
