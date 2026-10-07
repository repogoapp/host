// Package securechan is the phone-to-host end-to-end channel: Noise XX with an
// ML-KEM step (HFS) and the paired Ed25519 identities each signing the
// handshake transcript, which holds its fresh X25519 static key.
// SecureChannel.swift in RemoteTransport mirrors this file function for function.
package securechan

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/mlkem"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
)

const (
	protocolName = "Noise_XXhfs_25519+MLKEM1024_AESGCM_SHA256"
	prologueTag  = "repogo-e2e-v1"
	proofTag     = "repogo-noise-proof-v1"

	// The role goes under the signature so one side's proof can never serve
	// as the other's.
	roleInitiator byte = 1
	roleResponder byte = 2

	dhLen     = 32
	tagLen    = 16
	kemKeyLen = mlkem.EncapsulationKeySize1024
	kemCTLen  = mlkem.CiphertextSize1024
	proofLen  = ed25519.PublicKeySize + ed25519.SignatureSize

	// Every handshake message has one exact length: the payloads are empty
	// (message 1) or a proof, so anything else is malformed.
	msg1Len = dhLen + kemKeyLen
	msg2Len = dhLen + (kemCTLen + tagLen) + (dhLen + tagLen) + (proofLen + tagLen)
	msg3Len = (dhLen + tagLen) + (proofLen + tagLen)

	// maxKeyBytes caps what one key opens. GCM's bound tightens with the data
	// under a key; past this the session drops and the phone handshakes again.
	maxKeyBytes = 1 << 40
)

var (
	errAuth   = errors.New("securechan: authentication failed")
	errLength = errors.New("securechan: malformed handshake message")
	errProof  = errors.New("securechan: identity proof rejected")
	errSpent  = errors.New("securechan: session key used up")
	errOrder  = errors.New("securechan: handshake step out of order")
)

// cipherState is one direction's key and counter. The nonce is the counter,
// so wire order must be seal order.
type cipherState struct {
	aead   cipher.AEAD
	n      uint64
	opened uint64
}

func (c *cipherState) init(key []byte) error {
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	c.aead, c.n, c.opened = aead, 0, 0
	return nil
}

// nonce is 4 zero bytes and the counter big-endian (Noise §12.4). The last
// counter value is reserved by Noise §5.1, so a key never reaches it.
func (c *cipherState) nonce() ([]byte, error) {
	if c.n == math.MaxUint64 {
		return nil, errSpent
	}
	var n [12]byte
	binary.BigEndian.PutUint64(n[4:], c.n)
	c.n++
	return n[:], nil
}

func (c *cipherState) encrypt(ad, plain []byte) ([]byte, error) {
	nonce, err := c.nonce()
	if err != nil {
		return nil, err
	}
	return c.aead.Seal(nil, nonce, plain, ad), nil
}

func (c *cipherState) decrypt(ad, ct []byte) ([]byte, error) {
	nonce, err := c.nonce()
	if err != nil {
		return nil, err
	}
	if c.opened+uint64(len(ct)) > maxKeyBytes {
		return nil, errSpent
	}
	plain, err := c.aead.Open(nil, nonce, ct, ad)
	if err != nil {
		return nil, errAuth
	}
	c.opened += uint64(len(ct))
	return plain, nil
}

// symmetricState is the Noise chaining key and transcript hash.
type symmetricState struct {
	cs     cipherState
	hasKey bool
	ck     []byte
	h      []byte
}

func newSymmetricState(prologue []byte) *symmetricState {
	h := initialHash(protocolName)
	s := &symmetricState{ck: append([]byte(nil), h...), h: h}
	s.mixHash(prologue)
	return s
}

// initialHash is Noise §5.2: a name that fits in a hash is zero-padded, and a
// longer one is hashed.
func initialHash(name string) []byte {
	if len(name) <= sha256.Size {
		h := make([]byte, sha256.Size)
		copy(h, name)
		return h
	}
	sum := sha256.Sum256([]byte(name))
	return sum[:]
}

func hkdf(ck, ikm []byte) (out1, out2 []byte) {
	mac := hmac.New(sha256.New, ck)
	mac.Write(ikm)
	temp := mac.Sum(nil)
	mac = hmac.New(sha256.New, temp)
	mac.Write([]byte{1})
	out1 = mac.Sum(nil)
	mac.Reset()
	mac.Write(out1)
	mac.Write([]byte{2})
	out2 = mac.Sum(nil)
	return out1, out2
}

func (s *symmetricState) mixKey(ikm []byte) error {
	var k []byte
	s.ck, k = hkdf(s.ck, ikm)
	s.hasKey = true
	return s.cs.init(k)
}

func (s *symmetricState) mixHash(data []byte) {
	sum := sha256.New()
	sum.Write(s.h)
	sum.Write(data)
	s.h = sum.Sum(nil)
}

// transcript is a copy of h, since the next step moves it.
func (s *symmetricState) transcript() []byte { return append([]byte(nil), s.h...) }

func (s *symmetricState) encryptAndHash(plain []byte) ([]byte, error) {
	if !s.hasKey {
		s.mixHash(plain)
		return plain, nil
	}
	ct, err := s.cs.encrypt(s.h, plain)
	if err != nil {
		return nil, err
	}
	s.mixHash(ct)
	return ct, nil
}

func (s *symmetricState) decryptAndHash(ct []byte) ([]byte, error) {
	if !s.hasKey {
		s.mixHash(ct)
		return ct, nil
	}
	plain, err := s.cs.decrypt(s.h, ct)
	if err != nil {
		return nil, err
	}
	s.mixHash(ct)
	return plain, nil
}

func (s *symmetricState) split() (c1, c2 cipherState, err error) {
	k1, k2 := hkdf(s.ck, nil)
	if err := c1.init(k1); err != nil {
		return c1, c2, err
	}
	return c1, c2, c2.init(k2)
}

// handshakeState walks the XXhfs pattern:
// -> e, e1 / <- e, ee, ekem1, s, es / -> s, se.
type handshakeState struct {
	ss        *symmetricState
	initiator bool
	s, e      *ecdh.PrivateKey
	rs, re    *ecdh.PublicKey
	// e1 is the initiator's ML-KEM key; re1 is the responder's copy of its
	// public half. Both are fresh per handshake, like e.
	e1  *mlkem.DecapsulationKey1024
	re1 *mlkem.EncapsulationKey1024
	// encapsulate is the responder's ekem1. Only tests replace it, to fix
	// ML-KEM's randomized encapsulation for known answers.
	encapsulate func(*mlkem.EncapsulationKey1024) (sharedKey, ciphertext []byte)
}

// newHandshake takes the initiator's ML-KEM key in e1; the responder passes nil.
func newHandshake(initiator bool, prologue []byte, s, e *ecdh.PrivateKey, e1 *mlkem.DecapsulationKey1024) *handshakeState {
	return &handshakeState{
		ss: newSymmetricState(prologue), initiator: initiator, s: s, e: e, e1: e1,
		encapsulate: func(ek *mlkem.EncapsulationKey1024) ([]byte, []byte) { return ek.Encapsulate() },
	}
}

func prologue(initiatorID, responderID []byte) []byte {
	p := []byte(prologueTag)
	p = append(p, initiatorID...)
	return append(p, responderID...)
}

func generateKey() *ecdh.PrivateKey {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		panic(err)
	}
	return k
}

func generateKEMKey() *mlkem.DecapsulationKey1024 {
	k, err := mlkem.GenerateKey1024()
	if err != nil {
		panic(err)
	}
	return k
}

func (h *handshakeState) dh(priv *ecdh.PrivateKey, pub *ecdh.PublicKey) error {
	shared, err := priv.ECDH(pub)
	if err != nil {
		return errAuth
	}
	return h.ss.mixKey(shared)
}

func (h *handshakeState) readKey(msg []byte) (*ecdh.PublicKey, []byte, error) {
	if len(msg) < dhLen {
		return nil, nil, errLength
	}
	pub, err := ecdh.X25519().NewPublicKey(msg[:dhLen])
	if err != nil {
		return nil, nil, errLength
	}
	return pub, msg[dhLen:], nil
}

// writeMessage1 is the initiator's "e, e1". No key exists yet, so e1 and the
// empty payload are only hashed.
func (h *handshakeState) writeMessage1() ([]byte, error) {
	if h.e1 == nil {
		return nil, errOrder
	}
	e := h.e.PublicKey().Bytes()
	h.ss.mixHash(e)
	e1, err := h.ss.encryptAndHash(h.e1.EncapsulationKey().Bytes())
	if err != nil {
		return nil, err
	}
	payload, err := h.ss.encryptAndHash(nil)
	if err != nil {
		return nil, err
	}
	return append(append(e, e1...), payload...), nil
}

func (h *handshakeState) readMessage1(msg []byte) error {
	if len(msg) != msg1Len {
		return errLength
	}
	re, rest, err := h.readKey(msg)
	if err != nil {
		return err
	}
	h.re = re
	h.ss.mixHash(re.Bytes())
	e1, err := h.ss.decryptAndHash(rest)
	if err != nil {
		return err
	}
	if h.re1, err = mlkem.NewEncapsulationKey1024(e1); err != nil {
		return errLength
	}
	_, err = h.ss.decryptAndHash(nil)
	return err
}

// writeMessage2 is the responder's "e, ee, ekem1, s, es" with its identity
// proof; ekem1 encrypts the ciphertext first, then mixes the KEM secret in.
// prove gets the transcript after the key tokens, so it signs every key here.
func (h *handshakeState) writeMessage2(prove func(transcript []byte) []byte) ([]byte, error) {
	if h.re == nil || h.re1 == nil {
		return nil, errOrder
	}
	e := h.e.PublicKey().Bytes()
	h.ss.mixHash(e)
	if err := h.dh(h.e, h.re); err != nil {
		return nil, err
	}
	kemKey, ct := h.encapsulate(h.re1)
	h.re1 = nil
	sealedCT, err := h.ss.encryptAndHash(ct)
	if err != nil {
		return nil, err
	}
	if err := h.ss.mixKey(kemKey); err != nil {
		return nil, err
	}
	static, err := h.ss.encryptAndHash(h.s.PublicKey().Bytes())
	if err != nil {
		return nil, err
	}
	if err := h.dh(h.s, h.re); err != nil {
		return nil, err
	}
	sealed, err := h.ss.encryptAndHash(prove(h.ss.transcript()))
	if err != nil {
		return nil, err
	}
	out := append(append(e, sealedCT...), static...)
	return append(out, sealed...), nil
}

// readMessage2 returns the payload and the transcript hash its sender signed
// over, taken before the payload itself was mixed in.
func (h *handshakeState) readMessage2(msg []byte) (payload, transcript []byte, err error) {
	if h.e1 == nil {
		return nil, nil, errOrder
	}
	if len(msg) != msg2Len {
		return nil, nil, errLength
	}
	re, rest, err := h.readKey(msg)
	if err != nil {
		return nil, nil, err
	}
	h.re = re
	h.ss.mixHash(re.Bytes())
	if err := h.dh(h.e, h.re); err != nil {
		return nil, nil, err
	}
	// A wrong ciphertext of the right length still decapsulates (ML-KEM's
	// implicit rejection) to a different key, so the next open fails instead.
	ct, err := h.ss.decryptAndHash(rest[:kemCTLen+tagLen])
	if err != nil {
		return nil, nil, err
	}
	kemKey, err := h.e1.Decapsulate(ct)
	h.e1 = nil
	if err != nil {
		return nil, nil, errLength
	}
	if err := h.ss.mixKey(kemKey); err != nil {
		return nil, nil, err
	}
	rest = rest[kemCTLen+tagLen:]
	sBytes, err := h.ss.decryptAndHash(rest[:dhLen+tagLen])
	if err != nil {
		return nil, nil, err
	}
	if h.rs, err = ecdh.X25519().NewPublicKey(sBytes); err != nil {
		return nil, nil, errLength
	}
	if err := h.dh(h.e, h.rs); err != nil {
		return nil, nil, err
	}
	transcript = h.ss.transcript()
	payload, err = h.ss.decryptAndHash(rest[dhLen+tagLen:])
	return payload, transcript, err
}

// writeMessage3 is the initiator's "s, se" with its identity proof; prove
// is as in writeMessage2.
func (h *handshakeState) writeMessage3(prove func(transcript []byte) []byte) ([]byte, error) {
	if h.re == nil {
		return nil, errOrder
	}
	static, err := h.ss.encryptAndHash(h.s.PublicKey().Bytes())
	if err != nil {
		return nil, err
	}
	if err := h.dh(h.s, h.re); err != nil {
		return nil, err
	}
	sealed, err := h.ss.encryptAndHash(prove(h.ss.transcript()))
	if err != nil {
		return nil, err
	}
	return append(static, sealed...), nil
}

func (h *handshakeState) readMessage3(msg []byte) (payload, transcript []byte, err error) {
	if len(msg) != msg3Len {
		return nil, nil, errLength
	}
	sBytes, err := h.ss.decryptAndHash(msg[:dhLen+tagLen])
	if err != nil {
		return nil, nil, err
	}
	if h.rs, err = ecdh.X25519().NewPublicKey(sBytes); err != nil {
		return nil, nil, errLength
	}
	if err := h.dh(h.e, h.rs); err != nil {
		return nil, nil, err
	}
	transcript = h.ss.transcript()
	payload, err = h.ss.decryptAndHash(msg[dhLen+tagLen:])
	return payload, transcript, err
}

// split hands out the transport keys: the initiator sends on the first.
func (h *handshakeState) split() (send, recv cipherState, err error) {
	c1, c2, err := h.ss.split()
	if h.initiator {
		return c1, c2, err
	}
	return c2, c1, err
}

// proof binds an identity to this handshake: the transcript hash already
// holds both ephemerals, the KEM ciphertext and the signer's static key, so
// the signature cannot serve another session, or the other role in this one.
func proof(sign func([]byte) []byte, identityPub ed25519.PublicKey, role byte, transcript []byte) []byte {
	return append(append([]byte(nil), identityPub...), sign(proofMessage(role, transcript))...)
}

func proofMessage(role byte, transcript []byte) []byte {
	msg := append([]byte(proofTag), role)
	return append(msg, transcript...)
}

// verifyProof checks the payload against the transcript readMessage2/3
// returned; the peer then checks the identity is the one it pinned or routed to.
func verifyProof(payload []byte, role byte, transcript []byte) (ed25519.PublicKey, error) {
	if len(payload) != proofLen {
		return nil, errProof
	}
	pub := ed25519.PublicKey(payload[:ed25519.PublicKeySize])
	msg := proofMessage(role, transcript)
	if !ed25519.Verify(pub, msg, payload[ed25519.PublicKeySize:]) {
		return nil, errProof
	}
	return pub, nil
}
