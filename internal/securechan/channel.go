package securechan

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"

	"github.com/repogo/host/internal/device"
)

// Frame kinds, the first byte of every routed payload.
const (
	Handshake byte = 0x01
	Record    byte = 0x02
	// Reset tells a phone the host has no session for it (the host restarted);
	// the phone reconnects and handshakes again.
	Reset byte = 0x03
)

// Message1Len distinguishes a phone starting over from one finishing.
const Message1Len = 1 + msg1Len

// RecordOverhead is what sealing adds to a message: frame kind, body codec, tag.
const RecordOverhead = 2 + tagLen

var (
	ErrNoSession = errors.New("securechan: no session with this device")
	errFrame     = errors.New("securechan: not a record")
)

// Session is one established channel. Seal and Open each run under their own
// direction's lock: the counter is the nonce, so order is everything.
type Session struct {
	Peer ed25519.PublicKey

	sendMu sync.Mutex
	send   cipherState
	recvMu sync.Mutex
	recv   cipherState
}

// Seal encrypts one message and hands the framed record to write while still
// holding the send lock, so nonce order is wire order. Compression runs before
// the lock is taken.
func (s *Session) Seal(plain []byte, write func([]byte) error) error {
	body := encodeBody(plain)
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	ct, err := s.send.encrypt(nil, body)
	if err != nil {
		return err
	}
	return write(append([]byte{Record}, ct...))
}

// Open decrypts one framed record. Any error means the session is dead.
func (s *Session) Open(wire []byte) ([]byte, error) {
	if len(wire) == 0 || wire[0] != Record {
		return nil, errFrame
	}
	s.recvMu.Lock()
	body, err := s.recv.decrypt(nil, wire[1:])
	s.recvMu.Unlock()
	if err != nil {
		return nil, err
	}
	return decodeBody(body)
}

// prologueFor binds both routing ids, so a relay cannot splice two sessions.
func prologueFor(initiator, responder device.ID) ([]byte, error) {
	i, err := initiator.Bytes()
	if err != nil {
		return nil, err
	}
	r, err := responder.Bytes()
	if err != nil {
		return nil, err
	}
	return prologue(i, r), nil
}

// Initiate runs the phone side inline: three ordered messages over send and
// recv, then a session whose peer is the pinned host key or nothing.
func Initiate(ctx context.Context, identity *device.Identity, host device.ID, hostPublic ed25519.PublicKey,
	send func(context.Context, []byte) error, recv func(context.Context) ([]byte, error)) (*Session, error) {
	pro, err := prologueFor(identity.ID, host)
	if err != nil {
		return nil, err
	}
	h := newHandshake(true, pro, generateKey(), generateKey(), generateKEMKey())
	msg1, err := h.writeMessage1()
	if err != nil {
		return nil, err
	}
	if err := send(ctx, append([]byte{Handshake}, msg1...)); err != nil {
		return nil, err
	}
	msg2, err := recvHandshake(ctx, recv)
	if err != nil {
		return nil, err
	}
	payload, err := h.readMessage2(msg2)
	if err != nil {
		return nil, err
	}
	peer, err := verifyProof(payload, h.rs)
	if err != nil {
		return nil, err
	}
	if !peer.Equal(hostPublic) {
		return nil, fmt.Errorf("securechan: host identity does not match the paired key")
	}
	msg3, err := h.writeMessage3(proof(identity.Sign, identity.Public, h.s))
	if err != nil {
		return nil, err
	}
	if err := send(ctx, append([]byte{Handshake}, msg3...)); err != nil {
		return nil, err
	}
	sess := &Session{Peer: peer}
	if sess.send, sess.recv, err = h.split(); err != nil {
		return nil, err
	}
	return sess, nil
}

// recvHandshake skips records a host sealed for a session this side no
// longer has; they were in flight before the new handshake reached it.
func recvHandshake(ctx context.Context, recv func(context.Context) ([]byte, error)) ([]byte, error) {
	for {
		msg, err := recv(ctx)
		if err != nil {
			return nil, err
		}
		if len(msg) > 0 && msg[0] == Handshake {
			return msg[1:], nil
		}
	}
}

// Responder is the host side of one in-progress handshake.
type Responder struct {
	h    *handshakeState
	peer device.ID
}

// Respond consumes message 1 from peer and returns message 2 to send back.
func Respond(identity *device.Identity, peer device.ID, msg1 []byte) (*Responder, []byte, error) {
	if len(msg1) == 0 || msg1[0] != Handshake {
		return nil, nil, errLength
	}
	pro, err := prologueFor(peer, identity.ID)
	if err != nil {
		return nil, nil, err
	}
	h := newHandshake(false, pro, generateKey(), generateKey(), nil)
	if err := h.readMessage1(msg1[1:]); err != nil {
		return nil, nil, err
	}
	msg2, err := h.writeMessage2(proof(identity.Sign, identity.Public, h.s))
	if err != nil {
		return nil, nil, err
	}
	return &Responder{h: h, peer: peer}, append([]byte{Handshake}, msg2...), nil
}

// Finish consumes message 3. The peer's identity must be the key its routing
// id is the fingerprint of; whether that id is paired is the caller's question.
func (r *Responder) Finish(msg3 []byte) (*Session, error) {
	if len(msg3) == 0 || msg3[0] != Handshake {
		return nil, errLength
	}
	payload, err := r.h.readMessage3(msg3[1:])
	if err != nil {
		return nil, err
	}
	peer, err := verifyProof(payload, r.h.rs)
	if err != nil {
		return nil, err
	}
	if device.IDFor(peer) != r.peer {
		return nil, fmt.Errorf("securechan: identity does not match device id %s", r.peer)
	}
	sess := &Session{Peer: peer}
	if sess.send, sess.recv, err = r.h.split(); err != nil {
		return nil, err
	}
	return sess, nil
}
