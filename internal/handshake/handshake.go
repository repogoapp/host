// Package handshake is the three messages that open a connection: a challenge
// notification, a hello request carrying the proof, and the accepted result.
package handshake

const (
	MethodChallenge = "hello.challenge"
	MethodHello     = "hello"

	// MethodGoodbye refuses a handshake that failed before its hello arrived,
	// so the client can tell a refusal from a dropped connection.
	MethodGoodbye = "goodbye"
)

type Challenge struct {
	// Base64 in JSON: encoding/json does that for []byte, on both ends.
	Nonce    []byte `json:"nonce"`
	ServerID string `json:"server_id"`

	// Lets a client detect gross clock skew early, and is part of the signed
	// message so a challenge cannot be replayed from another session.
	WallMS uint64 `json:"wall_ms"`
}

type Hello struct {
	DeviceID  string `json:"device_id"`
	PublicKey []byte `json:"public_key"`
	GroupID   string `json:"group_id"`
	Role      string `json:"role"`

	ClientVersion string `json:"client_version,omitempty"`
	Platform      string `json:"platform,omitempty"`
	Label         string `json:"label,omitempty"`

	// Ed25519 over device.ChallengeMessage(nonce, server_id, wall_ms): a tagged,
	// length-framed concatenation, independent of how this struct is encoded.
	// Canonical JSON does not exist and a signature must never need it.
	ChallengeSig []byte `json:"challenge_sig,omitempty"`

	// Localhost only: the token from the 0600 runtime file. Rejected on the relay path.
	LocalToken string `json:"local_token,omitempty"`
}

// Accepted is the result of a successful hello.
type Accepted struct {
	// Echoed, so a client can confirm which identity was accepted.
	DeviceID string `json:"device_id"`

	// The running build, not the server's identity, so a stale binary is visible.
	ServerVersion string `json:"server_version"`
}

type Goodbye struct {
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// Roles are descriptive, not load-bearing: routing is by exact device id and
// authority comes from the signature. A runtime owns a filesystem and serves
// RPC; a client calls and observes.
const (
	RoleRuntime = "runtime"
	RoleClient  = "client"
)

const ReasonAuthFailed = "auth_failed"
