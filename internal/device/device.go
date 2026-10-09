// Package device is identity: this machine's keypair and the devices allowed
// to talk to it. There are no accounts. Stored as JSON, not in the droppable
// SQLite cache, because losing it costs every pairing.
package device

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/repogo/host/internal/errkind"
)

// IDLen is how many bytes of the public key's digest form an id. The id is the
// key's fingerprint, so id and key can be checked against each other with no
// registry; 128 bits is plenty and reads in a log line.
const IDLen = 16

// ID is an opaque routing key. Never parse one for meaning: nothing about the
// platform, the pairing order, or the owner is encoded in it.
type ID string

// IDFor derives the id of a public key.
func IDFor(pub ed25519.PublicKey) ID {
	sum := sha256.Sum256(pub)
	return ID(hex.EncodeToString(sum[:IDLen]))
}

// ErrBadID is an id that is not IDLen hex-encoded bytes.
var ErrBadID = errors.New("device: id must be 16 hex-encoded bytes")

// Bytes is the raw id the relay routes by and the secure channel binds.
func (id ID) Bytes() ([]byte, error) {
	b, err := hex.DecodeString(string(id))
	if err != nil || len(b) != IDLen {
		return nil, ErrBadID
	}
	return b, nil
}

// IDFromBytes is the inverse of Bytes.
func IDFromBytes(b []byte) ID { return ID(hex.EncodeToString(b)) }

// Identity is this machine's own keypair.
type Identity struct {
	ID      ID
	Public  ed25519.PublicKey
	private ed25519.PrivateKey
}

func Generate() (*Identity, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &Identity{ID: IDFor(pub), Public: pub, private: priv}, nil
}

// Sign proves possession of the private key.
func (i *Identity) Sign(msg []byte) []byte { return ed25519.Sign(i.private, msg) }

// Peer is another device this one has paired with.
type Peer struct {
	ID       ID     `json:"id"`
	Public   []byte `json:"public_key"`
	Label    string `json:"label,omitempty"`
	Platform string `json:"platform,omitempty"`
	AddedAt  int64  `json:"added_at"`

	Push *PushTarget `json:"push,omitempty"`
	// PushToStart lets the host start a Live Activity on the phone; Activities
	// are the ones running, by chat id, each with its own update token.
	PushToStart *PushTarget           `json:"push_to_start,omitempty"`
	Activities  map[string]PushTarget `json:"activities,omitempty"`
}

type PushTarget struct {
	Token       string `json:"token"`
	Environment string `json:"environment"`
	// InstallID names the phone's app install, which outlives its pairings and
	// its tokens. Re-pairing leaves the old records behind, and one install
	// keeps its targets on only the newest of them.
	InstallID string `json:"install_id,omitempty"`
	// At is when the host saved the token, in Unix milliseconds. iOS ends a
	// Live Activity after eight hours, so an older activity token is dead.
	At int64 `json:"at,omitempty"`
	// Grant carries PushProof for this host and token; GrantedAt is when the
	// phone signed it. The relay requires the accompanying Apple evidence.
	Grant     []byte `json:"grant,omitempty"`
	GrantedAt int64  `json:"granted_at,omitempty"`
}

var (
	ErrUnknownDevice = errkind.New(errkind.NotFound, "device: unknown device")
	ErrRevokeSelf    = errkind.New(errkind.Invalid, "device: a device cannot revoke itself")
	ErrRevokeHost    = errkind.New(errkind.Invalid, "device: the host cannot be revoked")
	ErrBadKey        = errkind.New(errkind.Invalid, "device: public key is malformed or not the id's")
	ErrBadSignature  = errors.New("device: signature does not verify")
	ErrBadGrant      = errkind.New(errkind.Invalid, "device: push grant does not verify for this host and token")
)

// Verify checks a signature against a paired device. The id is checked against
// the key's fingerprint first, or a caller could pair someone else's id with
// their own key.
func (p Peer) Verify(claimed ID, msg, sig []byte) error {
	pub := ed25519.PublicKey(p.Public)
	if len(pub) != ed25519.PublicKeySize {
		return ErrUnknownDevice
	}
	if IDFor(pub) != claimed {
		return fmt.Errorf("device: id %s does not match its public key", claimed)
	}
	if !ed25519.Verify(pub, msg, sig) {
		return ErrBadSignature
	}
	return nil
}

// NonceLen is the challenge nonce's size. A signer refuses any other, so a
// server never chooses how much of the signed message it controls.
const NonceLen = 32

// challengeTag opens every login message. The signer writes it, so no server
// can hand out a nonce that makes these bytes read as another signature's
// message, such as securechan's identity proof.
const challengeTag = "repogo-login-v1"

var ErrBadNonce = errkind.New(errkind.Invalid, "device: challenge nonce is not 32 bytes")

// ChallengeMessage binds a signature to one server and one moment: the nonce
// stops replay, the server id stops presenting it to another relay. Fields
// are length-framed, never delimited, since the server id is the peer's text.
func ChallengeMessage(nonce []byte, serverID string, wallMS uint64) ([]byte, error) {
	if len(nonce) != NonceLen || len(serverID) > math.MaxUint16 {
		return nil, ErrBadNonce
	}
	msg := make([]byte, 0, len(challengeTag)+NonceLen+2+len(serverID)+8)
	msg = append(msg, challengeTag...)
	msg = append(msg, nonce...)
	msg = binary.BigEndian.AppendUint16(msg, uint16(len(serverID)))
	msg = append(msg, serverID...)
	msg = binary.BigEndian.AppendUint64(msg, wallMS)
	return msg, nil
}

// PushGrantMessage is what a phone signs to let one host push to one of its
// tokens: tag first, every field length-framed, so no other signature the
// identity key makes can read as a grant.
func PushGrantMessage(host ID, token, environment string, grantedMS int64) []byte {
	msg := []byte("repogo-push-grant-v1")
	for _, field := range []string{string(host), token, environment} {
		msg = binary.BigEndian.AppendUint16(msg, uint16(len(field)))
		msg = append(msg, field...)
	}
	return binary.BigEndian.AppendUint64(msg, uint64(grantedMS))
}

// GrantPush supplies the identity signature for Go test clients; production
// push also requires Apple evidence in the proof envelope.
func (i *Identity) GrantPush(host ID, target PushTarget) PushTarget {
	if target.GrantedAt == 0 {
		target.GrantedAt = time.Now().UnixMilli()
	}
	target.Grant, _ = json.Marshal(PushProof{Signature: i.Sign(PushGrantMessage(host, target.Token, target.Environment, target.GrantedAt))})
	return target
}
