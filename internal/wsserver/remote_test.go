package wsserver_test

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
	"log/slog"
	"math/big"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/hostclient"
	"github.com/repogoapp/host/internal/hosttls"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/passthrough"
	"github.com/repogoapp/host/internal/rpc/pairing"
	"github.com/repogoapp/host/internal/rpc/registry"
	"github.com/repogoapp/host/internal/testhost"
	"github.com/repogoapp/host/internal/testwait"
	"github.com/repogoapp/host/internal/wsserver"
)

// remote serves the host's /ws over its pinned TLS, as phones reach it through
// the passthrough relay, and returns the wss:// URL and the pin.
type remote struct {
	url     string
	pin     [32]byte
	cert    *hosttls.Cert
	keys    *hosttls.Keys
	srv     *wsserver.Server
	devices *device.Store
	pairer  *device.Pairer
}

func newRemote(t *testing.T) *remote { return newRemoteOn(t, nil) }

// newRemoteOn serves on lis when given (a passthrough Listener), else on a
// plain TCP listener of its own.
func newRemoteOn(t *testing.T, through func(host *device.Identity) net.Listener) *remote {
	t.Helper()
	th := testhost.New(t)
	keys, err := hosttls.Open(filepath.Join(t.TempDir(), "tls-key.json"), th.Devices.Identity())
	if err != nil {
		t.Fatal(err)
	}
	cert := keys.Current()
	th.Config.TLSPin = func() (device.SignedPin, bool) { return keys.Current().Pin, true }
	router, err := registry.New(th.Config)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := wsserver.New(wsserver.Config{
		Token: testhost.Token, ServerID: "testhost", Devices: th.Devices, Router: router,
		Pushes: emit.NewMux(), OnDisconnect: func(device.ID) {}, Log: slog.New(slog.DiscardHandler),
		Pairing: th.Config.Pairer.Accepting,
	})
	if err != nil {
		t.Fatal(err)
	}
	var lis net.Listener
	url := ""
	if through != nil {
		lis = through(th.Devices.Identity())
	} else {
		if lis, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			t.Fatal(err)
		}
		url = "wss://" + lis.Addr().String() + "/ws"
	}
	go srv.ServeRemote(tls.NewListener(lis, keys.Config()))
	t.Cleanup(func() { srv.Shutdown(context.Background()); lis.Close() })
	return &remote{url: url, pin: cert.SPKI, cert: cert, keys: keys, srv: srv, devices: th.Devices, pairer: th.Config.Pairer}
}

func (r *remote) pair(t *testing.T) *device.Identity {
	t.Helper()
	id, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.devices.Add(device.Peer{ID: id.ID, Public: id.Public, Label: "Phone", Client: device.Client{Platform: "ios"}}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (r *remote) dial(t *testing.T, id *device.Identity, pin [32]byte, opts ...func(*hostclient.Config)) (*hostclient.Client, error) {
	t.Helper()
	cfg := hostclient.Config{
		URL: r.url, Identity: id, GroupID: r.devices.GroupID(), Platform: "ios", Label: "Phone",
		TLS: &tls.Config{
			MinVersion: tls.VersionTLS13,
			// The pin is the only trust: CA checks are off and the key must match.
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 || sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo) != pin {
					return errors.New("certificate does not match the pin")
				}
				return nil
			},
		},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := hostclient.Dial(ctx, cfg)
	if err == nil {
		t.Cleanup(func() { c.Close() })
	}
	return c, err
}

func TestRemotePhoneConnectsOverPinnedTLS(t *testing.T) {
	r := newRemote(t)
	c, err := r.dial(t, r.pair(t), r.pin)
	if err != nil {
		t.Fatal(err)
	}
	// The pin a phone paired before passthrough would fetch, signed by the host.
	var pin device.SignedPin
	if err := call(t, c, "host.tls_pin", nil, &pin); err != nil {
		t.Fatal(err)
	}
	if err := device.VerifyPin(r.devices.Identity().Public, pin); err != nil || [32]byte(pin.SPKI) != r.pin {
		t.Fatalf("host.tls_pin = %+v, %v", pin, err)
	}
}

func TestRemoteRefusesAWrongPin(t *testing.T) {
	r := newRemote(t)
	if _, err := r.dial(t, r.pair(t), [32]byte{1}); err == nil {
		t.Fatal("connected with a wrong pin")
	}
}

func TestRemoteHelloMustBindTheSession(t *testing.T) {
	r := newRemote(t)
	// A valid signature over the challenge alone is what a stolen TLS key
	// would let someone relay from another session.
	if _, err := r.dial(t, r.pair(t), r.pin, func(c *hostclient.Config) { c.Unbound = true }); err == nil {
		t.Fatal("a hello not bound to the TLS session was accepted")
	}
}

func TestRemoteRefusesTheLocalToken(t *testing.T) {
	r := newRemote(t)
	stranger, _ := device.Generate()
	if _, err := r.dial(t, stranger, r.pin, func(c *hostclient.Config) {
		c.Unsigned, c.LocalToken = true, testhost.Token
	}); err == nil {
		t.Fatal("the loopback token worked over passthrough")
	}
}

func TestRemoteRefusesAnUnpairedDevice(t *testing.T) {
	r := newRemote(t)
	stranger, _ := device.Generate()
	if _, err := r.dial(t, stranger, r.pin); err == nil {
		t.Fatal("an unpaired device connected")
	}
}

func TestRemoteThroughThePassthroughRelay(t *testing.T) {
	relayCert, relayPool := testCert(t, "h.test")
	relay := &passthrough.Relay{
		Suffix: ".h.test", LinkName: "h.test", DialTimeout: 5 * time.Second, Log: slog.New(slog.DiscardHandler),
		LinkTLS: &tls.Config{Certificates: []tls.Certificate{relayCert}},
	}
	relayLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go relay.Serve(t.Context(), relayLis)
	var hostID device.ID
	r := newRemoteOn(t, func(host *device.Identity) net.Listener {
		hostID = host.ID
		return passthrough.Listen(t.Context(), &passthrough.Listener{
			RelayAddr: relayLis.Addr().String(), Identity: host, Log: slog.New(slog.DiscardHandler),
			LinkTLS: &tls.Config{RootCAs: relayPool, ServerName: "h.test"},
		})
	})
	testwait.For(t, "the host's link", func() bool { return relay.Linked(hostID) })
	// The phone names the host; every byte goes to the relay's address.
	name := string(hostID) + ".h.test"
	r.url = "wss://" + name + "/ws"
	c, err := r.dial(t, r.pair(t), r.pin, func(cfg *hostclient.Config) {
		cfg.TLS.ServerName = name
		cfg.Dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, relayLis.Addr().String())
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	var pin device.SignedPin
	if err := call(t, c, "host.tls_pin", nil, &pin); err != nil {
		t.Fatal(err)
	}
}

func testCert(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
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
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// pairOver completes a pairing as the app does over passthrough: a stranger's
// bound hello, then pair.complete with the code's proof.
func (r *remote) pairOver(t *testing.T, id *device.Identity, code string, claim device.ID) error {
	t.Helper()
	c, err := r.dial(t, id, r.pin)
	if err != nil {
		return err
	}
	return call(t, c, "pair.complete", pairing.CompleteParams{
		DeviceID: string(claim), PublicKey: id.Public, Label: "iPhone", Platform: "ios",
		Proof: device.Proof(code, id.Public),
	}, nil)
}

func TestAStrangerPairsOverPassthroughWhileACodeIsLive(t *testing.T) {
	r := newRemote(t)
	invite, err := r.pairer.Begin("wss://relay.test/ws")
	if err != nil {
		t.Fatal(err)
	}
	phone, _ := device.Generate()
	if err := r.pairOver(t, phone, invite.Code, phone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.devices.Peer(phone.ID); err != nil {
		t.Fatal("pairing over passthrough didn't add the device")
	}
	// Paired now, it connects as any paired device does.
	if _, err := r.dial(t, phone, r.pin); err != nil {
		t.Fatalf("the new device can't connect: %v", err)
	}
}

func TestAStrangerIsRefusedWithoutALiveCode(t *testing.T) {
	r := newRemote(t)
	phone, _ := device.Generate()
	if _, err := r.dial(t, phone, r.pin); err == nil {
		t.Fatal("a stranger said hello with no pairing code live")
	}
}

func TestAStrangerGetsOnlyPairComplete(t *testing.T) {
	r := newRemote(t)
	invite, _ := r.pairer.Begin("wss://relay.test/ws")
	phone, _ := device.Generate()
	c, err := r.dial(t, phone, r.pin)
	if err != nil {
		t.Fatal(err)
	}
	if err := call(t, c, "devices.list", nil, nil); err == nil {
		t.Fatal("a stranger called a paired-only method")
	}
	// Someone else's key can't be paired on this stranger's socket.
	other, _ := device.Generate()
	if err := r.pairOver(t, phone, invite.Code, other.ID); err == nil {
		t.Fatal("a stranger paired an id that isn't its own key")
	}
	if _, err := r.devices.Peer(other.ID); err == nil {
		t.Fatal("the other id was added")
	}
}

// A rotated key: phones on passthrough are dropped, the old pin is refused from
// then on, and host.tls_pin hands out the new one with a higher counter.
func TestRotatingTheTLSKey(t *testing.T) {
	r := newRemote(t)
	phone := r.pair(t)
	before, err := r.dial(t, phone, r.pin)
	if err != nil {
		t.Fatal(err)
	}
	next, err := r.keys.Rotate()
	if err != nil {
		t.Fatal(err)
	}
	r.srv.CloseRemote()
	testwait.For(t, "the old session to drop", func() bool { return call(t, before, "host.tls_pin", nil, nil) != nil })
	if _, err := r.dial(t, phone, r.pin); err == nil {
		t.Fatal("the old pin still connects after rotation")
	}
	after, err := r.dial(t, phone, next.SPKI)
	if err != nil {
		t.Fatalf("the new pin doesn't connect: %v", err)
	}
	var pin device.SignedPin
	if err := call(t, after, "host.tls_pin", nil, &pin); err != nil || pin.Counter != 2 || [32]byte(pin.SPKI) != next.SPKI {
		t.Fatalf("host.tls_pin after rotation = %+v, %v", pin, err)
	}
}

func TestRemoteReadsUpToTheHostsLimit(t *testing.T) {
	// A host on a custom relay raises its limit; the direct route must too.
	jsonrpc.SetLimit(jsonrpc.CustomRelayMessageBytes)
	t.Cleanup(func() { jsonrpc.SetLimit(jsonrpc.MaxMessageBytes) })
	r := newRemote(t)
	c, err := r.dial(t, r.pair(t), r.pin)
	if err != nil {
		t.Fatal(err)
	}
	// Past the RepoGo relay's 8 MiB: the method may refuse it, the socket may not.
	big := strings.Repeat("x", jsonrpc.MaxMessageBytes+1<<20)
	_ = call(t, c, "host.tls_pin", map[string]string{"pad": big}, nil)
	var pin device.SignedPin
	if err := call(t, c, "host.tls_pin", nil, &pin); err != nil {
		t.Fatalf("the connection did not survive a message over 8 MiB: %v", err)
	}
}
