package device

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/apphome"
	"github.com/repogoapp/host/internal/errkind"
)

// Store holds this device's identity and everyone it has paired with. The
// private key is a 0600 file, not a keychain item.
type Store struct {
	path string

	mu       sync.RWMutex
	identity *Identity
	peers    map[ID]Peer
	groupID  string
	revoked  []func(ID)
}

type fileFormat struct {
	GroupID    string `json:"group_id"`
	DeviceID   ID     `json:"device_id"`
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"`
	Peers      []Peer `json:"peers"`
}

// Open loads the identity file, generating one on first run.
func Open(path string) (*Store, error) {
	s := &Store{path: path, peers: map[ID]Peer{}}
	var f fileFormat
	found, err := apphome.ReadJSON(path, &f)
	if err == nil && found {
		err = s.load(f)
	}
	if err != nil {
		// Refuse rather than silently regenerating. A new identity would
		// invalidate every pairing, which is not a recovery a user expects.
		return nil, fmt.Errorf("device: %s is unreadable: %w", path, err)
	}
	if !found {
		if err := s.initialize(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) load(f fileFormat) error {
	priv, err := base64.StdEncoding.DecodeString(f.PrivateKey)
	if err != nil || len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("bad private key")
	}
	pub, err := base64.StdEncoding.DecodeString(f.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("bad public key")
	}

	s.identity = &Identity{ID: IDFor(pub), Public: pub, private: priv}
	if s.identity.ID != f.DeviceID {
		return fmt.Errorf("device id %s does not match its key", f.DeviceID)
	}
	s.groupID = f.GroupID
	for _, p := range f.Peers {
		s.peers[p.ID] = p
	}
	return nil
}

func (s *Store) initialize() error {
	id, err := Generate()
	if err != nil {
		return err
	}
	s.identity = id
	// A device is its own group until it pairs with something. The group id is
	// random rather than derived from the device id so that pairing two devices
	// can later merge them under one id without either being "the original".
	s.groupID = randomID()
	return s.save()
}

func (s *Store) Identity() *Identity {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.identity
}

func (s *Store) GroupID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.groupID
}

// Peer returns a paired device.
func (s *Store) Peer(id ID) (Peer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.peers[id]
	if !ok {
		return Peer{}, ErrUnknownDevice
	}
	return p, nil
}

func (s *Store) Peers() []Peer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, p)
	}
	return out
}

// Add records a paired device.
func (s *Store) Add(p Peer) error {
	if pub := ed25519.PublicKey(p.Public); len(pub) != ed25519.PublicKeySize || IDFor(pub) != p.ID {
		return ErrBadKey.Errorf("%s", p.ID)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if p.AddedAt == 0 {
		p.AddedAt = time.Now().UnixMilli()
	}
	return s.commit(func() { s.peers[p.ID] = p })
}

// OnRevoke adds fn to what runs once a device is revoked, outside the lock, so
// each transport can end the sessions that device still holds.
func (s *Store) OnRevoke(fn func(ID)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revoked = append(s.revoked, fn)
}

// Revoke deletes a paired device and its push targets; its next hello is
// refused as unknown. caller is the device asking, which removes itself by
// forgetting the host instead.
func (s *Store) Revoke(caller, id ID) error {
	listeners, err := s.revoke(caller, id)
	for _, fn := range listeners {
		fn(id)
	}
	return err
}

// revoke returns the listeners to tell once the lock is released.
func (s *Store) revoke(caller, id ID) ([]func(ID), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch id {
	case caller:
		return nil, ErrRevokeSelf
	case s.identity.ID:
		return nil, ErrRevokeHost
	}
	if _, ok := s.peers[id]; !ok {
		return nil, ErrUnknownDevice
	}
	if err := s.commit(func() { delete(s.peers, id) }); err != nil {
		return nil, err
	}
	return slices.Clone(s.revoked), nil
}

// The RegisterPush kinds: the alert token is the empty kind.
const (
	// PushToStart is the token that starts the phone's Live Activity.
	PushToStart = "start"
	// PushActivity is the update token of the Live Activity running.
	PushActivity = "activity"
	// PushStepsStart is the token that starts a plugin's step activity.
	PushStepsStart = "steps_start"
	// PushSteps is one step activity's update token, saved by its key with
	// RegisterStepsPush.
	PushSteps = "steps"
)

// RegisterPush saves one of the phone's tokens by kind. An empty token
// removes it, so an ended activity needs no other call.
func (s *Store) RegisterPush(id ID, kind string, target PushTarget) error {
	if kind == PushSteps {
		return fmt.Errorf("%w: a steps token is saved by its activity's key", errkind.ErrInvalid)
	}
	if err := s.checkPush(id, kind, target); err != nil {
		return err
	}
	return s.updatePush(id, func(p *Peer) {
		field := pushField(p, kind)
		if target.Token == "" {
			*field = nil
			return
		}
		// The host stamps when it first saw a token; the phone saves the same
		// one again on every reconnect, which must not restart the clock.
		target.At = time.Now().UnixMilli()
		if old := *field; old != nil && old.Token == target.Token {
			target.At = old.At
		}
		*field = &target
		if kind != PushActivity {
			s.release(id, kind, target)
		}
	})
}

// RegisterStepsPush saves the update token of the step activity key names;
// an empty token removes it.
func (s *Store) RegisterStepsPush(id ID, key string, target PushTarget) error {
	if key == "" || len(key) > maxActivityKey {
		return fmt.Errorf("%w: a steps token needs its activity's key, at most %d bytes", errkind.ErrInvalid, maxActivityKey)
	}
	if err := s.checkPush(id, PushSteps, target); err != nil {
		return err
	}
	return s.updatePush(id, func(p *Peer) {
		steps := maps.Clone(p.Steps)
		if target.Token == "" {
			delete(steps, key)
		} else {
			target.At = time.Now().UnixMilli()
			if old, ok := steps[key]; ok && old.Token == target.Token {
				target.At = old.At
			}
			if steps == nil {
				steps = map[string]PushTarget{}
			}
			steps[key] = target
		}
		p.Steps = steps
	})
}

// ForgetStepsPush drops a step activity's dead token, unless the phone has
// replaced it meanwhile.
func (s *Store) ForgetStepsPush(id ID, key, token string) error {
	return s.updatePush(id, func(p *Peer) {
		if old, ok := p.Steps[key]; ok && old.Token == token {
			steps := maps.Clone(p.Steps)
			delete(steps, key)
			p.Steps = steps
		}
	})
}

// checkPush validates a token and, unless it is the empty "forget it",
// the grant the phone signed for it.
func (s *Store) checkPush(id ID, kind string, target PushTarget) error {
	if err := target.validate(kind); err != nil {
		return err
	}
	if target.Token == "" {
		return nil
	}
	peer, err := s.Peer(id)
	if err != nil {
		return err
	}
	msg := PushGrantMessage(s.identity.ID, target.Token, target.Environment, target.GrantedAt)
	if _, err := VerifyPushProof(peer.Public, msg, target.Grant); err != nil {
		return ErrBadGrant
	}
	return nil
}

// SetClient saves what a device says it is. The phone says it on every
// connect, so an unchanged client writes nothing.
func (s *Store) SetClient(id ID, c Client) error {
	if err := c.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[id]
	if !ok {
		return ErrUnknownDevice
	}
	if p.Client.Platform == c.Platform && p.Client.Kind == c.Kind && p.Client.Version == c.Version &&
		p.Client.Build == c.Build && maps.Equal(p.Client.Features, c.Features) {
		return nil
	}
	return s.commit(func() {
		p.Client = c
		s.peers[id] = p
	})
}

// ForgetPush drops a dead token of one kind, unless the phone has replaced
// it meanwhile.
func (s *Store) ForgetPush(id ID, kind, token string) error {
	return s.updatePush(id, func(p *Peer) {
		if field := pushField(p, kind); *field != nil && (*field).Token == token {
			*field = nil
		}
	})
}

// pushField is the peer's token of one kind.
func pushField(p *Peer, kind string) **PushTarget {
	switch kind {
	case PushToStart:
		return &p.PushToStart
	case PushActivity:
		return &p.Activity
	case PushStepsStart:
		return &p.StepsStart
	}
	return &p.Push
}

// maxInstallID bounds the one free-form string a phone stores here.
const maxInstallID = 64

// maxActivityKey bounds a step activity's key: its plugin and id.
const maxActivityKey = 256

// The services a push token belongs to: Apple's for iOS, Firebase Cloud
// Messaging for Android.
const (
	PushAPNs = "apns"
	PushFCM  = "fcm"
)

// maxFCMToken bounds an FCM registration token, which runs to a few hundred bytes.
const maxFCMToken = 1024

func (t PushTarget) validate(kind string) error {
	switch {
	case kind != "" && kind != PushToStart && kind != PushActivity && kind != PushStepsStart && kind != PushSteps:
		return fmt.Errorf("%w: kind must be start, activity, steps_start, steps or empty", errkind.ErrInvalid)
	case len(t.InstallID) > maxInstallID:
		return fmt.Errorf("%w: install_id is too long", errkind.ErrInvalid)
	case t.Token == "":
		return nil
	}
	switch t.Service {
	case PushAPNs:
		if _, err := hex.DecodeString(t.Token); err != nil {
			return fmt.Errorf("%w: token must be hexadecimal", errkind.ErrInvalid)
		}
		if t.Environment != "sandbox" && t.Environment != "production" {
			return fmt.Errorf("%w: environment must be sandbox or production", errkind.ErrInvalid)
		}
	case PushFCM:
		// FCM has no Live Activities and no sandbox.
		if kind != "" {
			return fmt.Errorf("%w: an fcm token is an alert token", errkind.ErrInvalid)
		}
		if t.Environment != "" {
			return fmt.Errorf("%w: an fcm token has no environment", errkind.ErrInvalid)
		}
		if !validFCMToken(t.Token) {
			return fmt.Errorf("%w: token is not an fcm registration token", errkind.ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: service must be apns or fcm", errkind.ErrInvalid)
	}
	if len(t.Grant) == 0 || len(t.Grant) > MaxPushProofBytes || t.GrantedAt <= 0 {
		return fmt.Errorf("%w: a token needs the phone's grant", errkind.ErrInvalid)
	}
	return nil
}

// release clears target from every other peer with its token or install, since
// a re-paired phone's old records would each get a copy of every push. Caller
// holds the write lock.
func (s *Store) release(owner ID, kind string, target PushTarget) {
	for id, p := range s.peers {
		t := pushField(&p, kind)
		if id == owner || *t == nil {
			continue
		}
		if (*t).Token == target.Token || (target.InstallID != "" && (*t).InstallID == target.InstallID) {
			*t = nil
			s.peers[id] = p
		}
	}
}

func (s *Store) updatePush(id ID, update func(*Peer)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[id]
	if !ok {
		return ErrUnknownDevice
	}
	return s.commit(func() {
		update(&p)
		s.peers[id] = p
	})
}

// commit applies change and saves, restoring the peers if the save fails so
// memory never admits what the file does not. Caller holds the write lock.
func (s *Store) commit(change func()) error {
	before := maps.Clone(s.peers)
	change()
	if err := s.save(); err != nil {
		s.peers = before
		return err
	}
	return nil
}

// Verify authenticates a connecting device against the paired set.
func (s *Store) Verify(id ID, msg, sig []byte) error {
	p, err := s.Peer(id)
	if err != nil {
		return err
	}
	return p.Verify(id, msg, sig)
}

// save assumes the caller holds the write lock, or is initializing.
func (s *Store) save() error {
	f := fileFormat{
		GroupID:    s.groupID,
		DeviceID:   s.identity.ID,
		PublicKey:  base64.StdEncoding.EncodeToString(s.identity.Public),
		PrivateKey: base64.StdEncoding.EncodeToString(s.identity.private),
	}
	for _, p := range s.peers {
		f.Peers = append(f.Peers, p)
	}

	// 0600 from the first byte: the private key is never readable by others.
	return apphome.WriteJSON(s.path, f, 0o600)
}

func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// validFCMToken is the shape Firebase issues: URL-safe base64 with a colon
// between the instance and the token.
func validFCMToken(token string) bool {
	if len(token) > maxFCMToken {
		return false
	}
	for _, r := range token {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == ':') {
			return false
		}
	}
	return true
}
