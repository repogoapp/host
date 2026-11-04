package device

import (
	"slices"

	"github.com/repogoapp/host/internal/errkind"
)

// Rotation moves a paired device to a new key without pairing again (plan
// 520): the old key signs for the new one, as the host's identity signs its pin.

const rotateTag = "repogo-rotate-v1"

var (
	ErrRotateKey   = errkind.New(errkind.Invalid, "device: the new key is malformed or already paired")
	ErrRotateProof = errkind.New(errkind.Denied, "device: rotation is not signed by both the old and the new key")
)

// RotateMessage is what both keys sign: the host, so the request can't be
// replayed to another host the phone is paired with, the old id, and the new
// key, length-framed because the two key kinds differ in size.
func RotateMessage(host, old ID, newPublic []byte) ([]byte, error) {
	h, err := host.Bytes()
	if err != nil {
		return nil, err
	}
	o, err := old.Bytes()
	if err != nil {
		return nil, err
	}
	if len(newPublic) > 255 {
		return nil, ErrRotateKey
	}
	msg := make([]byte, 0, len(rotateTag)+2*IDLen+1+len(newPublic))
	msg = append(msg, rotateTag...)
	msg = append(msg, h...)
	msg = append(msg, o...)
	msg = append(msg, byte(len(newPublic)))
	return append(msg, newPublic...), nil
}

// Rotate gives old's device newPublic as its key, and so a new id, keeping its
// label and client; push tokens go, since the old key signed their grants and
// the phone sends new ones on connect. Retire then tells the rest of the host.
func (s *Store) Rotate(old ID, newPublic, oldSig, newSig []byte) (Peer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[old]
	if !ok {
		return Peer{}, ErrUnknownDevice
	}
	newID := IDFor(newPublic)
	if !ValidPhoneKey(newPublic) || newID == old || s.peers[newID].ID != "" || newID == s.identity.ID {
		return Peer{}, ErrRotateKey
	}
	msg, err := RotateMessage(s.identity.ID, old, newPublic)
	if err != nil {
		return Peer{}, err
	}
	if !VerifyPhone(p.Public, msg, oldSig) || !VerifyPhone(newPublic, msg, newSig) {
		return Peer{}, ErrRotateProof
	}
	next := Peer{
		ID: newID, Public: slices.Clone(newPublic), Label: p.Label, Client: p.Client, AddedAt: p.AddedAt,
	}
	if err := s.commit(func() {
		delete(s.peers, old)
		s.peers[newID] = next
	}); err != nil {
		return Peer{}, err
	}
	return next, nil
}

// Retire tells the host an id is gone, as Revoke does: its sockets close and
// paired devices refresh their lists. For an id Rotate already removed.
func (s *Store) Retire(id ID) {
	s.mu.Lock()
	listeners := slices.Clone(s.revoked)
	s.mu.Unlock()
	for _, fn := range listeners {
		fn(id)
	}
}
