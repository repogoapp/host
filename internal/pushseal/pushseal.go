// Package pushseal seals what every push carries (alerts, Live Activities,
// Live Updates) with a secret one phone shares with this host, so the
// relay, Apple and Google pass ciphertext. docs/push-seal.md is the format; the phones'
// PushSeal implementations open what Seal makes.
package pushseal

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
)

// Version is the box version this host seals.
const Version = 1

// The levels of a phone's push_seal feature: at FeatureLiveActivities it opens
// Live Activity boxes (and, on Android, alerts); at FeatureAlerts an iPhone's
// notification service extension opens alerts too.
const (
	FeatureLiveActivities = 1
	FeatureAlerts         = 2
)

// SecretSize is the length of the secret a phone sends.
const SecretSize = 32

const (
	tag       = "repogo-push-seal-v1"
	nonceSize = 12
	// The frame pads its body to a multiple of this, so small changes in
	// counts or text don't show in the length.
	padTo = 32
	// formatJSON is the frame's only format.
	formatJSON = 1
)

// The purposes a box is sealed for; with the bind, they are its associated
// data, so a box opens only where it was meant to.
const (
	AgentsState = "agents.state"
	StepsAttrs  = "steps.attrs"
	StepsState  = "steps.state"
	FCM         = "fcm"
	// Alert is an iOS alert's title, body, group and tap fields.
	Alert = "alert"
)

// Envelope is a sealed body as it travels: in an iOS content-state or
// attributes, or flattened into an FCM data map.
type Envelope struct {
	V   int    `json:"v"`
	KID string `json:"kid"`
	Box string `json:"box"`
}

var errOpen = errors.New("pushseal: box does not open")

// KeyID names secret without revealing it: the first 4 bytes of a tagged
// SHA-256, in hex.
func KeyID(secret []byte) string {
	sum := sha256.Sum256(append([]byte(tag+" kid"), secret...))
	return hex.EncodeToString(sum[:4])
}

// Seal encrypts body's JSON for host, purpose and bind under secret.
func Seal(secret []byte, host, purpose, bind string, body any) (Envelope, error) {
	plain, err := json.Marshal(body)
	if err != nil {
		return Envelope{}, err
	}
	return sealBytes(secret, host, purpose, bind, plain, rand.Read)
}

func sealBytes(secret []byte, host, purpose, bind string, plain []byte, random func([]byte) (int, error)) (Envelope, error) {
	aead, err := newAEAD(secret)
	if err != nil {
		return Envelope{}, err
	}
	if len(plain) > 0xffff {
		return Envelope{}, fmt.Errorf("pushseal: body is %d bytes", len(plain))
	}
	nonce := make([]byte, nonceSize, nonceSize+len(plain)+padTo+aead.Overhead())
	if _, err := random(nonce); err != nil {
		return Envelope{}, err
	}
	box := aead.Seal(nonce, nonce, frame(plain), associated(host, purpose, bind))
	return Envelope{V: Version, KID: KeyID(secret), Box: base64.RawURLEncoding.EncodeToString(box)}, nil
}

// Open is Seal's inverse, for tests and the vectors: phones open their own.
func Open(secret []byte, host, purpose, bind string, e Envelope) ([]byte, error) {
	if e.V != Version || e.KID != KeyID(secret) {
		return nil, errOpen
	}
	aead, err := newAEAD(secret)
	if err != nil {
		return nil, err
	}
	box, err := base64.RawURLEncoding.DecodeString(e.Box)
	if err != nil || len(box) < nonceSize+aead.Overhead() {
		return nil, errOpen
	}
	framed, err := aead.Open(nil, box[:nonceSize], box[nonceSize:], associated(host, purpose, bind))
	if err != nil || len(framed) < 3 || framed[0] != formatJSON {
		return nil, errOpen
	}
	n := int(binary.BigEndian.Uint16(framed[1:3]))
	if 3+n > len(framed) {
		return nil, errOpen
	}
	return framed[3 : 3+n], nil
}

// newAEAD is AES-256-GCM under the key HKDF derives from secret.
func newAEAD(secret []byte) (cipher.AEAD, error) {
	if len(secret) != SecretSize {
		return nil, fmt.Errorf("pushseal: secret must be %d bytes", SecretSize)
	}
	key, err := hkdf.Key(sha256.New, secret, nil, tag+" enc", 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// CollapseID is the APNs collapse id for id: a keyed hash, since Apple reads
// the header, so a later alert still replaces an earlier one without naming
// the chat it belongs to.
func CollapseID(secret []byte, id string) string {
	if id == "" {
		return ""
	}
	key, err := hkdf.Key(sha256.New, secret, nil, tag+" collapse", 32)
	if err != nil {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id))
	return hex.EncodeToString(mac.Sum(nil)[:16])
}

// frame is the plaintext: format, body length, body, zeros to a multiple of padTo.
func frame(body []byte) []byte {
	n := 3 + len(body)
	padded := (n + padTo - 1) / padTo * padTo
	out := make([]byte, 3, padded)
	out[0] = formatJSON
	binary.BigEndian.PutUint16(out[1:], uint16(len(body)))
	out = append(out, body...)
	return append(out, make([]byte, padded-n)...)
}

// associated is the tag, then host, purpose and bind, each length-framed as
// device.PushGrantMessage frames its fields.
func associated(host, purpose, bind string) []byte {
	out := []byte(tag)
	for _, field := range []string{host, purpose, bind} {
		out = binary.BigEndian.AppendUint16(out, uint16(len(field)))
		out = append(out, field...)
	}
	return out
}
