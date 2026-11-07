package testhost

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/host"
	"github.com/repogoapp/host/internal/hostclient"
	"github.com/repogoapp/host/internal/hosttls"
	"github.com/repogoapp/host/internal/passthrough"
	"github.com/repogoapp/host/internal/rpc"
	"github.com/repogoapp/host/internal/wsserver"
)

// Token is the loopback token Serve's WebSocket server accepts.
const Token = "test-token"

// Transports is a router served the ways a client reaches it: the loopback
// WebSocket, and the host's /ws over its pinned TLS, as phones reach it
// through the relay.
type Transports struct {
	WS, Remote string // ws:// and wss:// URLs
	Pin        [32]byte
}

// Serve starts both transports for router.
func Serve(t testing.TB, devices *device.Store, router *rpc.Router) *Transports {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := wsserver.New(wsserver.Config{
		Token: Token, ServerID: "testhost", Devices: devices, Router: router,
		Pushes: emit.NewMux(), OnDisconnect: func(device.ID) {}, Log: log,
	})
	if err != nil {
		t.Fatalf("wsserver: %v", err)
	}
	wsAddr, err := srv.Listen()
	if err != nil {
		t.Fatalf("wsserver listen: %v", err)
	}
	keys, err := hosttls.Open(filepath.Join(t.TempDir(), "tls-key.json"), devices.Identity())
	if err != nil {
		t.Fatalf("host TLS key: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.ServeRemote(lis, keys.Config())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		lis.Close()
	})
	return &Transports{
		WS:     fmt.Sprintf("ws://%s/ws", wsAddr.(*net.TCPAddr)),
		Remote: "wss://" + lis.Addr().String() + "/ws",
		Pin:    keys.Current().SPKI,
	}
}

// PinnedTLS trusts only the host key whose SPKI hash is pin, as a phone does.
func PinnedTLS(pin [32]byte) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		// The pin is the only trust: CA checks are off and the key must match.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo) != pin {
				return errors.New("certificate does not match the pin")
			}
			return nil
		},
	}
}

// Pin is the hash of the TLS key a host with a relay serves phones.
func (h *Host) Pin(t testing.TB) [32]byte {
	t.Helper()
	keys, err := hosttls.Open(filepath.Join(h.State, "tls-key.json"), h.Devices.Identity())
	if err != nil {
		t.Fatalf("host TLS key: %v", err)
	}
	return keys.Current().SPKI
}

// RelayName is the local relay's name, as h.repogo.app is RepoGo's: a host
// name, since TLS sends no server name for an address.
const RelayName = "localhost"

// Relay is a passthrough relay on loopback, shut down when the test ends.
type Relay struct {
	*passthrough.Relay
	// Addr is what a host's Config.Relay names; Roots trusts its certificate.
	Addr  string
	Roots *x509.CertPool
}

// StartRelay runs a relay; adjust sets its fields, such as Push, first.
func StartRelay(t testing.TB, adjust ...func(*passthrough.Relay)) *Relay {
	t.Helper()
	cert, roots := selfSigned(t, RelayName)
	r := &passthrough.Relay{
		Suffix: "." + RelayName, LinkName: RelayName, DialTimeout: 5 * time.Second,
		LinkTLS: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13},
		Log:     slog.New(slog.DiscardHandler),
	}
	for _, f := range adjust {
		f(r)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go r.Serve(ctx, lis)
	_, port, _ := net.SplitHostPort(lis.Addr().String())
	return &Relay{Relay: r, Addr: net.JoinHostPort(RelayName, port), Roots: roots}
}

// On is a New option linking the host to r.
func (r *Relay) On(c *host.Config) { c.Relay, c.RelayRoots = r.Addr, r.Roots }

// Through points cfg at host through the relay, trusting only pin.
func (r *Relay) Through(cfg hostclient.Config, host device.ID, pin [32]byte) hostclient.Config {
	name := string(host) + "." + RelayName
	_, port, _ := net.SplitHostPort(r.Addr)
	cfg.URL = "wss://" + net.JoinHostPort(name, port) + "/ws"
	cfg.TLS = PinnedTLS(pin)
	cfg.TLS.ServerName = name
	cfg.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	}
	return cfg
}

func selfSigned(t testing.TB, name string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}
