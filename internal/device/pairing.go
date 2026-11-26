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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/apphome"
	"github.com/repogoapp/host/internal/errkind"
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

// LegacyGroupID fills Invite.GroupID. Remove it once no App Store build up to
// 1.0.4 pairs.
const LegacyGroupID = "repogo"

// Invite is what the QR encodes.
type Invite struct {
	// Where to reach the host. For local pairing this is the loopback address;
	// for remote it is the relay's address.
	Address string `json:"address"`

	// GroupID is always LegacyGroupID: App Store builds up to 1.0.4 refuse an
	// invite without one. Nothing reads it.
	GroupID string `json:"group_id"`

	// The inviting device, so the joiner can verify the host's reply came from
	// the machine whose screen it just scanned.
	InviterID     ID     `json:"inviter_id"`
	InviterPublic []byte `json:"inviter_public"`
	// Label is the machine's name, shown before a link pairs: a sent link
	// must not join a device to a machine it never saw named.
	Label string `json:"label,omitempty"`

	Code      string `json:"code"`
	ExpiresAt int64  `json:"expires_at"`

	// TLSPin, when the host has a relay, is the TLS key phones pin, signed by
	// InviterPublic: a phone reaches the host only with it.
	TLSPin *SignedPin `json:"tls_pin,omitempty"`
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
	// name is what an invite calls this host: hostinfo.Service.Name.
	name func() string

	mu      sync.Mutex
	code    string
	expires time.Time
	// inviteID is the asking device's tag for code, empty for the CLI's; it
	// moves with code so a replaced code never reports another's join.
	inviteID  string
	reusable  reusableCode
	tlsPin    *SignedPin
	completed []func(inviteID string)
}

// SetTLSPin puts the host's signed TLS pin in every invite from now on.
func (p *Pairer) SetTLSPin(pin SignedPin) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tlsPin = &pin
}

// TLSPin is the pin phones dial now; nil without a relay.
func (p *Pairer) TLSPin() *SignedPin {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tlsPin
}

type reusableCode struct {
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expires_at"`

	// TLSPin, when the host has a relay, is the TLS key phones pin, signed by
	// InviterPublic: a phone reaches the host only with it.
	TLSPin *SignedPin `json:"tls_pin,omitempty"`
}

// OpenPairer loads the reusable invite saved at reusablePath, if any, so a
// restart does not end it.
func OpenPairer(s *Store, reusablePath string, name func() string) (*Pairer, error) {
	p := &Pairer{store: s, reusablePath: reusablePath, name: name}
	if _, err := apphome.ReadJSON(reusablePath, &p.reusable); err != nil {
		return nil, err
	}
	return p, nil
}

// Begin issues an invite, replacing any pairing already in progress. A device
// tags it with inviteID, which OnComplete's listeners hear once a device
// joins with it; the CLI passes none.
func (p *Pairer) Begin(address, inviteID string) (Invite, error) {
	code := newPairingCode()
	expires := time.Now().Add(codeTTL)
	p.mu.Lock()
	p.code, p.expires, p.inviteID = code, expires, inviteID
	pin := p.tlsPin
	p.mu.Unlock()
	return p.invite(address, code, expires, pin), nil
}

// Cancel ends the one-time pairing if code is still the one in progress, for
// an invite that never reached anyone; a newer one is left alone.
func (p *Pairer) Cancel(code string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.code == code {
		p.code, p.inviteID = "", ""
	}
}

// OnComplete adds fn to what runs, outside the lock, once a device pairs:
// with the one-time code's invite ID, or empty for the CLI's or the reusable.
func (p *Pairer) OnComplete(fn func(inviteID string)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.completed = append(p.completed, fn)
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
	return p.invite(address, reusable.Code, time.UnixMilli(reusable.ExpiresAt), p.tlsPin), nil
}

// RevokeReusable ends the reusable invite. Devices that joined with it stay
// paired; Remove removes them.
func (p *Pairer) RevokeReusable() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.endReusable()
}

// Remove revokes a device and ends every invite it may hold: the reusable
// one and the pending one-time code, which a device could ask for and keep
// to pair back in once revoked. Complete holds the same lock, so the device
// cannot pair back in between.
func (p *Pairer) Remove(caller, id ID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// The invites end first: a revoke that then fails can be retried, but a
	// revoked device must never be left an invite to pair back in with.
	p.code, p.inviteID = "", ""
	if err := p.endReusable(); err != nil {
		return err
	}
	return p.store.Revoke(caller, id)
}

// endReusable assumes the caller holds p.mu.
func (p *Pairer) endReusable() error {
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

func (p *Pairer) invite(address, code string, expires time.Time, pin *SignedPin) Invite {
	id := p.store.Identity()
	return Invite{
		Address:       address,
		GroupID:       LegacyGroupID,
		InviterID:     id.ID,
		InviterPublic: id.Public,
		Label:         p.name(),
		Code:          code,
		ExpiresAt:     expires.UnixMilli(),
		TLSPin:        pin,
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

// Accepting reports whether a stranger may pair now: a one-time code or the
// reusable invite is live.
func (p *Pairer) Accepting() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return (p.code != "" && !time.Now().After(p.expires)) || p.reusableLive()
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
	hostProof, inviteID, listeners, err := p.complete(joiner, proof)
	for _, fn := range listeners {
		fn(inviteID)
	}
	return hostProof, err
}

// complete returns the listeners to tell once the lock is released.
func (p *Pairer) complete(joiner Peer, proof []byte) ([]byte, string, []func(string), error) {
	// Hold through persistence so simultaneous requests cannot consume one code twice.
	p.mu.Lock()
	defer p.mu.Unlock()

	once := p.code != "" && !time.Now().After(p.expires)
	reusable := p.reusableLive()
	// Constant time, or the comparison leaks how much of a guessed proof was right.
	switch {
	case once && hmac.Equal(proof, Proof(p.code, joiner.Public)):
		if err := p.store.Add(joiner); err != nil {
			return nil, "", nil, err
		}
		// One code admits one device. Leaving it live would let anyone who
		// photographed the screen join later, and the user has no way to know.
		code, inviteID := p.code, p.inviteID
		p.code, p.inviteID = "", ""
		return Proof(code, p.store.Identity().Public), inviteID, slices.Clone(p.completed), nil
	case reusable && hmac.Equal(proof, Proof(p.reusable.Code, joiner.Public)):
		if err := p.store.Add(joiner); err != nil {
			return nil, "", nil, err
		}
		return Proof(p.reusable.Code, p.store.Identity().Public), "", slices.Clone(p.completed), nil
	case once || reusable:
		return nil, "", nil, ErrBadProof
	case p.code != "" || p.reusable.Code != "":
		p.code, p.inviteID = "", ""
		return nil, "", nil, ErrPairingExpired
	default:
		return nil, "", nil, ErrNoPairing
	}
}
