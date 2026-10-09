// Package appattest verifies the Apple-certified app instance behind a push grant.
package appattest

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/asn1"
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"github.com/fxamacker/cbor/v2"
)

// Public trust anchor from https://www.apple.com/certificateauthority/.
//
//go:embed apple_root.pem
var appleRoot []byte

var ErrProof = errors.New("push grant is not from the expected Apple-attested app")

const identityTag = "repogo-push-app-v1"

type Verifier struct {
	roots  *x509.CertPool
	rpID   [32]byte
	decode cbor.DecMode
}

func New(appID string) (*Verifier, error) {
	prefix, bundle, ok := strings.Cut(appID, ".")
	if !ok || len(prefix) != 10 || bundle == "" {
		return nil, errors.New("appattest: App ID prefix and bundle ID are required")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(appleRoot) {
		return nil, errors.New("appattest: invalid Apple trust anchor")
	}
	decode, err := (cbor.DecOptions{
		DupMapKey: cbor.DupMapKeyEnforcedAPF, MaxNestedLevels: 8,
		MaxArrayElements: 16, MaxMapPairs: 32, IndefLength: cbor.IndefLengthForbidden,
		TagsMd: cbor.TagsForbidden,
	}).DecMode()
	if err != nil {
		return nil, err
	}
	return &Verifier{roots: roots, rpID: sha256.Sum256([]byte(appID)), decode: decode}, nil
}

type attestation struct {
	Format    string `cbor:"fmt"`
	AuthData  []byte `cbor:"authData"`
	Statement struct {
		Certificates [][]byte `cbor:"x5c"`
	} `cbor:"attStmt"`
}

type assertion struct {
	Signature []byte `cbor:"signature"`
	AuthData  []byte `cbor:"authenticatorData"`
}

// Verify accepts a reusable, time-bounded authorization, not a one-shot login.
// The attested key is bound to the identity; its assertion binds the exact grant.
// Certificates are checked at signedAt, the grant's own signed time.
func (v *Verifier) Verify(identity, message, object, signed []byte, environment string, signedAt time.Time) error {
	if len(identity) != 32 || len(object) > 16<<10 || len(signed) > 4<<10 {
		return ErrProof
	}
	key, err := v.key(identity, object, environment, signedAt)
	if err != nil {
		return ErrProof
	}
	var a assertion
	if v.decode.Unmarshal(signed, &a) != nil || len(a.AuthData) < 37 ||
		!bytes.Equal(a.AuthData[:32], v.rpID[:]) || binary.BigEndian.Uint32(a.AuthData[33:37]) == 0 {
		return ErrProof
	}
	if err := v.extensions(a.AuthData[37:], a.AuthData[32], environment); err != nil {
		return err
	}
	// The key signs the nonce as a message, so ECDSA hashes it once more.
	nonce := digest(a.AuthData, message)
	signedHash := sha256.Sum256(nonce[:])
	if !ecdsa.VerifyASN1(key, signedHash[:], a.Signature) {
		return ErrProof
	}
	return nil
}

func digest(authData, clientData []byte) [32]byte {
	hash := sha256.Sum256(clientData)
	b := append(append([]byte(nil), authData...), hash[:]...)
	return sha256.Sum256(b)
}

func (v *Verifier) key(identity, object []byte, environment string, signedAt time.Time) (*ecdsa.PublicKey, error) {
	var a attestation
	if v.decode.Unmarshal(object, &a) != nil || a.Format != "apple-appattest" ||
		len(a.Statement.Certificates) < 2 || len(a.Statement.Certificates) > 4 || len(a.AuthData) < 87 {
		return nil, ErrProof
	}
	leaf, err := x509.ParseCertificate(a.Statement.Certificates[0])
	if err != nil {
		return nil, ErrProof
	}
	intermediates := x509.NewCertPool()
	for _, raw := range a.Statement.Certificates[1:] {
		cert, err := x509.ParseCertificate(raw)
		if err != nil {
			return nil, ErrProof
		}
		intermediates.AddCert(cert)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: v.roots, Intermediates: intermediates,
		CurrentTime: signedAt, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		return nil, ErrProof
	}
	key, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, ErrProof
	}
	wantNonce := digest(a.AuthData, append([]byte(identityTag), identity...))
	nonceOK := false
	for _, ext := range leaf.Extensions {
		if !ext.Id.Equal(asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 8, 2}) {
			continue
		}
		var value struct {
			Nonce []byte `asn1:"explicit,tag:1"`
		}
		rest, err := asn1.Unmarshal(ext.Value, &value)
		nonceOK = err == nil && len(rest) == 0 && bytes.Equal(value.Nonce, wantNonce[:])
	}
	keyID := sha256.Sum256(elliptic.Marshal(key.Curve, key.X, key.Y))
	if !nonceOK || !bytes.Equal(a.AuthData[:32], v.rpID[:]) || a.AuthData[32]&0x40 == 0 ||
		binary.BigEndian.Uint32(a.AuthData[33:37]) != 0 || binary.BigEndian.Uint16(a.AuthData[53:55]) != 32 ||
		!bytes.Equal(a.AuthData[55:87], keyID[:]) {
		return nil, ErrProof
	}
	aaguid := "appattest\x00\x00\x00\x00\x00\x00\x00"
	switch environment {
	case "sandbox":
		aaguid = "appattestdevelop"
	case "production":
	default:
		return nil, ErrProof
	}
	if string(a.AuthData[37:53]) != aaguid {
		return nil, ErrProof
	}
	var cose struct {
		Type      int    `cbor:"1,keyasint"`
		Algorithm int    `cbor:"3,keyasint"`
		Curve     int    `cbor:"-1,keyasint"`
		X         []byte `cbor:"-2,keyasint"`
		Y         []byte `cbor:"-3,keyasint"`
	}
	rest, err := v.decode.UnmarshalFirst(a.AuthData[87:], &cose)
	if err != nil || cose.Type != 2 || cose.Algorithm != -7 || cose.Curve != 1 ||
		!bytes.Equal(cose.X, key.X.FillBytes(make([]byte, 32))) || !bytes.Equal(cose.Y, key.Y.FillBytes(make([]byte, 32))) {
		return nil, ErrProof
	}
	if err := v.extensions(rest, a.AuthData[32], environment); err != nil {
		return nil, err
	}
	return key, nil
}

func (v *Verifier) extensions(raw []byte, flags byte, environment string) error {
	if flags&0x80 == 0 {
		if len(raw) != 0 {
			return ErrProof
		}
		return nil
	}
	// Apple encodes the category as four little-endian bytes, not a CBOR integer.
	var ext struct {
		Category []byte `cbor:"apple_validation_category_01"`
		Version  string `cbor:"apple_bundle_version_01"`
	}
	if v.decode.Unmarshal(raw, &ext) != nil || len(ext.Category) != 4 || ext.Version == "" {
		return ErrProof
	}
	category := binary.LittleEndian.Uint32(ext.Category)
	if environment == "sandbox" && category == 3 || environment == "production" && (category == 2 || category == 4) {
		return nil
	}
	return ErrProof
}
