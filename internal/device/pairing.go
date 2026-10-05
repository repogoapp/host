package device

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/repogo/host/internal/apphome"
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

	// MaxReusableTTL bounds a reusable invite, so one forgotten on a machine
	// stops admitting devices on its own.
	MaxReusableTTL = 30 * 24 * time.Hour
)

var (
	ErrNoPairing      = errkind.New(errkind.NotFound, "pairing: no pairing in progress")
	ErrPairingExpired = errkind.New(errkind.Denied, "pairing: code expired")
	ErrBadProof       = errkind.New(errkind.Denied, "pairing: proof does not match the code")
	ErrReusableTTL    = errkind.New(errkind.Invalid, "pairing: a reusable invite lasts from an hour to 30 days")
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
// tell which is live. The reusable invite sits beside it: a code many devices
// may join with until it expires, for a machine nobody stands at.
type Pairer struct {
	store        *Store
	reusablePath string

	mu       sync.Mutex
	code     string
	expires  time.Time
	reusable reusableCode
}

type reusableCode struct {
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expires_at"`
}

// OpenPairer loads the reusable invite saved at reusablePath, if any, so a
// restart does not end it.
func OpenPairer(s *Store, reusablePath string) (*Pairer, error) {
	p := &Pairer{store: s, reusablePath: reusablePath}
	if _, err := apphome.ReadJSON(reusablePath, &p.reusable); err != nil {
		return nil, err
	}
	return p, nil
}

// Begin issues an invite, replacing any pairing already in progress.
func (p *Pairer) Begin(address string) (Invite, error) {
	code := newPairingCode()
	expires := time.Now().Add(codeTTL)
	p.mu.Lock()
	p.code, p.expires = code, expires
	p.mu.Unlock()
	return p.invite(address, code, expires), nil
}

// BeginReusable issues an invite any number of devices may join with until
// ttl passes, replacing the reusable invite already open. The one-time
// pairing is left as it is.
func (p *Pairer) BeginReusable(address string, ttl time.Duration) (Invite, error) {
	if ttl < time.Hour || ttl > MaxReusableTTL {
		return Invite{}, ErrReusableTTL
	}
	reusable := reusableCode{Code: newPairingCode(), ExpiresAt: time.Now().Add(ttl).UnixMilli()}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := apphome.WriteJSON(p.reusablePath, reusable, 0o600); err != nil {
		return Invite{}, err
	}
	p.reusable = reusable
	return p.invite(address, reusable.Code, time.UnixMilli(reusable.ExpiresAt)), nil
}

// RevokeReusable ends the reusable invite. Devices that joined with it stay
// paired; Store.Revoke removes them.
func (p *Pairer) RevokeReusable() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := os.Remove(p.reusablePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	p.reusable = reusableCode{}
	return nil
}

// ReusableExpires is when the reusable invite ends; zero when there is none.
func (p *Pairer) ReusableExpires() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.reusableLive() {
		return time.Time{}
	}
	return time.UnixMilli(p.reusable.ExpiresAt)
}

func (p *Pairer) invite(address, code string, expires time.Time) Invite {
	id := p.store.Identity()
	return Invite{
		Address:       address,
		GroupID:       p.store.GroupID(),
		InviterID:     id.ID,
		InviterPublic: id.Public,
		Code:          code,
		ExpiresAt:     expires.UnixMilli(),
	}
}

// Caller holds mu.
func (p *Pairer) reusableLive() bool {
	return p.reusable.Code != "" && time.Now().UnixMilli() <= p.reusable.ExpiresAt
}

func newPairingCode() string {
	code := make([]byte, codeBytes)
	rand.Read(code)
	return base64.RawURLEncoding.EncodeToString(code)
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

// Complete admits a device that proved it saw a code and returns the host's
// own proof, so the joiner knows it paired with the machine it scanned.
func (p *Pairer) Complete(joiner Peer, proof []byte) ([]byte, error) {
	// Hold through persistence so simultaneous requests cannot consume one code twice.
	p.mu.Lock()
	defer p.mu.Unlock()

	once := p.code != "" && !time.Now().After(p.expires)
	reusable := p.reusableLive()
	// Constant time, or the comparison leaks how much of a guessed proof was right.
	switch {
	case once && hmac.Equal(proof, Proof(p.code, joiner.Public)):
		if err := p.store.Add(joiner); err != nil {
			return nil, err
		}
		// One code admits one device. Leaving it live would let anyone who
		// photographed the screen join later, and the user has no way to know.
		code := p.code
		p.code = ""
		return Proof(code, p.store.Identity().Public), nil
	case reusable && hmac.Equal(proof, Proof(p.reusable.Code, joiner.Public)):
		if err := p.store.Add(joiner); err != nil {
			return nil, err
		}
		return Proof(p.reusable.Code, p.store.Identity().Public), nil
	case once || reusable:
		return nil, ErrBadProof
	case p.code != "" || p.reusable.Code != "":
		p.code = ""
		return nil, ErrPairingExpired
	default:
		return nil, ErrNoPairing
	}
}
