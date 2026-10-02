package device

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/repogo/host/internal/errkind"
)

// Pairing is the QR handshake that admits a new device. No PAKE: a QR code is a
// high-entropy, visually-confirmed channel. The code is a shared secret both
// sides HMAC their key with, proving each saw the screen.

const (
	// codeTTL is short because the window is "how long between showing a QR and
	// scanning it" — long enough for a person, short enough that a code left on
	// a screen goes stale.
	codeTTL = 2 * time.Minute

	codeBytes = 32
)

var (
	ErrNoPairing      = errkind.New(errkind.NotFound, "pairing: no pairing in progress")
	ErrPairingExpired = errkind.New(errkind.Denied, "pairing: code expired")
	ErrBadProof       = errkind.New(errkind.Denied, "pairing: proof does not match the code")
)

// Invite is what the QR encodes.
type Invite struct {
	// Where to reach the host. For local pairing this is the loopback address;
	// for remote it is the relay.
	Address string `json:"address"`

	GroupID string `json:"group_id"`

	// The inviting device, so the joiner can verify the host's reply came from
	// the machine whose screen it just scanned.
	InviterID     ID     `json:"inviter_id"`
	InviterPublic []byte `json:"inviter_public"`

	Code      string `json:"code"`
	ExpiresAt int64  `json:"expires_at"`
}

func (i Invite) Encode() (string, error) {
	b, err := json.Marshal(i)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// URL is the invite as an https link under pairHost: at once an App Clip
// invocation, a universal link into the app, and a browser fallback.
func (i Invite) URL(pairHost string) (string, error) {
	payload, err := i.Encode()
	if err != nil {
		return "", err
	}
	return strings.TrimRight(pairHost, "/") + "/p/" + payload, nil
}

// DecodeInvite accepts either the bare payload or its invocation URL.
func DecodeInvite(s string) (Invite, error) {
	if u, err := url.Parse(s); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
		s = path.Base(u.Path)
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return Invite{}, err
	}
	var i Invite
	err = json.Unmarshal(raw, &i)
	return i, err
}

// Pairer owns the one in-flight pairing; two open codes means a user cannot
// tell which is live.
type Pairer struct {
	store *Store

	mu      sync.Mutex
	code    string
	expires time.Time
}

func NewPairer(s *Store) *Pairer { return &Pairer{store: s} }

// Begin issues an invite, replacing any pairing already in progress.
func (p *Pairer) Begin(address string) (Invite, error) {
	code := make([]byte, codeBytes)
	rand.Read(code)
	encoded := base64.RawURLEncoding.EncodeToString(code)
	expires := time.Now().Add(codeTTL)

	id := p.store.Identity()
	invite := Invite{
		Address:       address,
		GroupID:       p.store.GroupID(),
		InviterID:     id.ID,
		InviterPublic: id.Public,
		Code:          encoded,
		ExpiresAt:     expires.UnixMilli(),
	}
	p.mu.Lock()
	p.code, p.expires = encoded, expires
	p.mu.Unlock()
	return invite, nil
}

// Pending reports whether a code is live, for status without leaking the code.
func (p *Pairer) Pending() (bool, time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.code == "" || time.Now().After(p.expires) {
		return false, time.Time{}
	}
	return true, p.expires
}

// Proof binds a joining device's public key to the pairing code. HMAC rather
// than a hash so only someone holding the code can produce or check it.
func Proof(code string, pub []byte) []byte {
	mac := hmac.New(sha256.New, []byte(code))
	mac.Write(pub)
	return mac.Sum(nil)
}

// Complete admits a device that proved it saw the code and returns the host's
// own proof, so the joiner knows it paired with the machine it scanned.
func (p *Pairer) Complete(joiner Peer, proof []byte) ([]byte, error) {
	// Hold through persistence so simultaneous requests cannot consume one code twice.
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.code == "" {
		return nil, ErrNoPairing
	}
	if time.Now().After(p.expires) {
		p.code = ""
		return nil, ErrPairingExpired
	}
	// Constant time, or the comparison leaks how much of a guessed proof was right.
	if !hmac.Equal(proof, Proof(p.code, joiner.Public)) {
		return nil, ErrBadProof
	}

	if err := p.store.Add(joiner); err != nil {
		return nil, err
	}
	// One code admits one device. Leaving it live would let anyone who
	// photographed the screen join later, and the user has no way to know.
	code := p.code
	p.code = ""

	return Proof(code, p.store.Identity().Public), nil
}
