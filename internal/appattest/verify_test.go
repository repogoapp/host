package appattest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"math/big"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
)

func TestVerifyBindsAppIdentityAndGrant(t *testing.T) {
	now := time.Now()
	v, err := New("ABCDEFGHIJ.app.repogo")
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Temporary test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	v.roots = x509.NewCertPool()
	v.roots.AddCert(ca)
	identity := make([]byte, 32)
	identity[0] = 42
	message := []byte("host/token/environment/time")
	for _, tc := range []struct {
		environment string
		// Newer iOS appends Apple's validation extensions to both objects.
		extensions bool
	}{{"production", false}, {"sandbox", false}, {"production", true}, {"sandbox", true}} {
		environment := tc.environment
		flags := byte(0x40)
		var extensions []byte
		if tc.extensions {
			category := []byte{2, 0, 0, 0}
			if environment == "sandbox" {
				category = []byte{3, 0, 0, 0}
			}
			flags |= 0x80
			extensions, err = cbor.Marshal(map[string]any{"apple_validation_category_01": category, "apple_bundle_version_01": "46"})
			if err != nil {
				t.Fatal(err)
			}
		}
		name := environment
		if tc.extensions {
			name += "/extensions"
		}
		t.Run(name, func(t *testing.T) {
			auth := make([]byte, 87)
			copy(auth, v.rpID[:])
			auth[32] = flags
			guid := "appattest\x00\x00\x00\x00\x00\x00\x00"
			if environment == "sandbox" {
				guid = "appattestdevelop"
			}
			copy(auth[37:53], guid)
			binary.BigEndian.PutUint16(auth[53:55], 32)
			keyID := sha256.Sum256(elliptic.Marshal(key.Curve, key.X, key.Y))
			copy(auth[55:], keyID[:])
			cose, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: key.X.FillBytes(make([]byte, 32)), -3: key.Y.FillBytes(make([]byte, 32))})
			if err != nil {
				t.Fatal(err)
			}
			auth = append(append(auth, cose...), extensions...)
			nonce := digest(auth, append([]byte(identityTag), identity...))
			ext, err := asn1.Marshal(struct {
				Nonce []byte `asn1:"explicit,tag:1"`
			}{nonce[:]})
			if err != nil {
				t.Fatal(err)
			}
			leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 2, 840, 113635, 100, 8, 2}, Value: ext}}}
			leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
			if err != nil {
				t.Fatal(err)
			}
			a := attestation{Format: "apple-appattest", AuthData: auth}
			a.Statement.Certificates = [][]byte{leafDER, caDER}
			object, err := cbor.Marshal(a)
			if err != nil {
				t.Fatal(err)
			}
			assertionFor := func(counter uint32, msg []byte) []byte {
				data := make([]byte, 37)
				copy(data, v.rpID[:])
				// Apple sets the attested-data bit on assertions too.
				data[32] = flags
				binary.BigEndian.PutUint32(data[33:], counter)
				data = append(data, extensions...)
				nonce := digest(data, msg)
				signedHash := sha256.Sum256(nonce[:])
				sig, err := ecdsa.SignASN1(rand.Reader, key, signedHash[:])
				if err != nil {
					t.Fatal(err)
				}
				raw, err := cbor.Marshal(assertion{Signature: sig, AuthData: data})
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			signed := assertionFor(1, message)
			if err := v.Verify(identity, message, object, signed, environment, now); err != nil {
				t.Fatalf("valid proof: %v", err)
			}
			badIdentity := append([]byte(nil), identity...)
			badIdentity[0]++
			for name, check := range map[string]func() error{
				"identity":            func() error { return v.Verify(badIdentity, message, object, signed, environment, now) },
				"grant":               func() error { return v.Verify(identity, []byte("other token"), object, signed, environment, now) },
				"zero counter":        func() error { return v.Verify(identity, message, object, assertionFor(0, message), environment, now) },
				"missing attestation": func() error { return v.Verify(identity, message, nil, signed, environment, now) },
				"malformed assertion": func() error { return v.Verify(identity, message, object, []byte{0xff}, environment, now) },
				"expired certificate": func() error { return v.Verify(identity, message, object, signed, environment, now.Add(2*time.Hour)) },
				"environment": func() error {
					other := "sandbox"
					if environment == other {
						other = "production"
					}
					return v.Verify(identity, message, object, signed, other, now)
				},
				"wrong app": func() error {
					other := *v
					other.rpID = sha256.Sum256([]byte("ABCDEFGHIJ.other"))
					return other.Verify(identity, message, object, signed, environment, now)
				},
				"untrusted certificate": func() error {
					other, err := New("ABCDEFGHIJ.app.repogo")
					if err != nil {
						t.Fatal(err)
					}
					return other.Verify(identity, message, object, signed, environment, now)
				},
			} {
				t.Run(name, func(t *testing.T) {
					if check() == nil {
						t.Fatal("accepted invalid proof")
					}
				})
			}
		})
	}
}
