// Package hosttls is the host's TLS key for passthrough connections: phones
// reach it through the relay and pin its public key, not a CA. The key lasts;
// the certificate around it is made fresh at every start.
package hosttls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/repogoapp/host/internal/apphome"
	"github.com/repogoapp/host/internal/device"
)

// Cert is the loaded key: its certificate, the pin phones hold, and that pin
// signed by the host's identity.
type Cert struct {
	TLS  tls.Certificate
	SPKI [sha256.Size]byte
	Pin  device.SignedPin
}

// stored is the file: the key, and the counter its pin carries. A new key
// takes the next counter, so phones drop the old pin for it.
type stored struct {
	Key     []byte `json:"key"`
	Counter int64  `json:"counter"`
}

// LoadOrCreate reads the key at path, making one on first run, and signs its
// pin with host. The file is 0600: whoever holds the key can be this host to
// a phone that only checks the pin.
func LoadOrCreate(path string, host *device.Identity) (*Cert, error) {
	var s stored
	found, err := apphome.ReadJSON(path, &s)
	if err != nil {
		return nil, fmt.Errorf("hosttls: read %s: %w", path, err)
	}
	if !found {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		s = stored{Key: der, Counter: nextCounter(0)}
		if err := apphome.WriteJSON(path, s, 0o600); err != nil {
			return nil, fmt.Errorf("hosttls: write %s: %w", path, err)
		}
	}
	parsed, err := x509.ParsePKCS8PrivateKey(s.Key)
	if err != nil {
		return nil, fmt.Errorf("hosttls: key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, errors.New("hosttls: key is not ECDSA")
	}
	return issue(key, s.Counter, host)
}

// nextCounter is the counter a new key takes: past the last one, and never
// below the time in seconds, so a host whose key file was lost (a reinstall)
// still starts above every pin its phones hold and they accept the new key.
func nextCounter(last int64) int64 {
	return max(last+1, time.Now().Unix())
}

func issue(key *ecdsa.PrivateKey, counter int64, host *device.Identity) (*Cert, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	// Phones check the key, not these dates; they only have to parse.
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "RepoGo host " + string(host.ID)},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	spki := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	pin, err := host.SignPin(spki, counter)
	if err != nil {
		return nil, err
	}
	return &Cert{
		TLS:  tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf},
		SPKI: spki,
		Pin:  pin,
	}, nil
}

// Config serves the certificate. TLS 1.3 only: every client this pin is for
// speaks it, and it is what carries the hybrid post-quantum key exchange.
func (c *Cert) Config() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{c.TLS},
		MinVersion:   tls.VersionTLS13,
		NextProtos:   []string{"http/1.1"},
	}
}

// Keys is the host's TLS key while it runs: the one it serves now, replaced in
// place by Rotate, so a new key needs no restart.
type Keys struct {
	path    string
	host    *device.Identity
	mu      sync.Mutex
	current atomic.Pointer[Cert]
}

// Open loads (or makes) the key at path.
func Open(path string, host *device.Identity) (*Keys, error) {
	c, err := LoadOrCreate(path, host)
	if err != nil {
		return nil, err
	}
	k := &Keys{path: path, host: host}
	k.current.Store(c)
	return k, nil
}

// Current is the key being served and its signed pin.
func (k *Keys) Current() *Cert { return k.current.Load() }

// Rotate replaces the key under the next counter, for one that may have
// leaked: phones refuse the new certificate, learn its pin over the relay
// channel, and never accept the old key again.
func (k *Keys) Rotate() (*Cert, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	next := nextCounter(k.Current().Pin.Counter)
	c, err := issue(key, next, k.host)
	if err != nil {
		return nil, err
	}
	if err := apphome.WriteJSON(k.path, stored{Key: der, Counter: next}, 0o600); err != nil {
		return nil, fmt.Errorf("hosttls: write %s: %w", k.path, err)
	}
	k.current.Store(c)
	return c, nil
}

// Config serves whichever key is current at each handshake.
func (k *Keys) Config() *tls.Config {
	return &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &k.Current().TLS, nil },
		MinVersion:     tls.VersionTLS13,
		NextProtos:     []string{"http/1.1"},
	}
}
