package wsserver_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/hostclient"
	"github.com/repogoapp/host/internal/hosttls"
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
	root    string
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
	th.Config.Pairer.SetTLSPin(cert.Pin)
	go srv.ServeRemote(tls.NewListener(lis, keys.Config()))
	t.Cleanup(func() { srv.Shutdown(context.Background()); lis.Close() })
	return &remote{url: url, pin: cert.SPKI, cert: cert, keys: keys, srv: srv, devices: th.Devices, pairer: th.Config.Pairer, root: th.Root}
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
	if err := call(t, c, "devices.list", nil, nil); err != nil {
		t.Fatal(err)
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
	if err := call(t, c, "devices.list", nil, nil); err != nil {
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

// pair.complete answers with the host identity's signature over the joiner's
// key and the pin it dialed, which only the real host can make: an invite
// built around a retired TLS key can't finish pairing.
func TestPairCompleteIsSignedByTheHost(t *testing.T) {
	r := newRemote(t)
	invite, err := r.pairer.Begin("wss://relay.test/ws")
	if err != nil {
		t.Fatal(err)
	}
	phone, _ := device.Generate()
	c, err := r.dial(t, phone, r.pin)
	if err != nil {
		t.Fatal(err)
	}
	var got pairing.CompleteResult
	if err := call(t, c, "pair.complete", pairing.CompleteParams{
		DeviceID: string(phone.ID), PublicKey: phone.Public, Label: "iPhone", Platform: "ios",
		Proof: device.Proof(invite.Code, phone.Public),
	}, &got); err != nil {
		t.Fatal(err)
	}
	msg, err := device.PairMessage(got.HostID, phone.Public, r.cert.Pin)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(got.HostPublic, msg, got.HostSignature) {
		t.Fatal("the host's signature doesn't cover this phone's key and the pin it dialed")
	}
	other, _ := device.Generate()
	if forged, _ := device.PairMessage(got.HostID, other.Public, r.cert.Pin); ed25519.Verify(got.HostPublic, forged, got.HostSignature) {
		t.Fatal("the signature also verifies for another phone's key")
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

// A rotated key: connected phones are dropped, the old pin is refused from then
// on, and the new key carries a higher counter.
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
	testwait.For(t, "the old session to drop", func() bool { return call(t, before, "devices.list", nil, nil) != nil })
	if _, err := r.dial(t, phone, r.pin); err == nil {
		t.Fatal("the old pin still connects after rotation")
	}
	if _, err := r.dial(t, phone, next.SPKI); err != nil {
		t.Fatalf("the new pin doesn't connect: %v", err)
	}
	if next.Pin.Counter <= r.cert.Pin.Counter {
		t.Fatalf("the new pin's counter is %d, not above %d", next.Pin.Counter, r.cert.Pin.Counter)
	}
}

// counted is a TCP connection that tallies the bytes it reads.
type counted struct {
	net.Conn
	n *atomic.Int64
}

func (c counted) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// A phone that asks for deflate gets a large page compressed on the wire; one
// that doesn't, as app 1.0.3, gets the same page as plain text.
func TestANegotiatedConnectionCarriesACompressedPage(t *testing.T) {
	r := newRemote(t)
	var rows []string
	for i := range 2000 {
		rows = append(rows, fmt.Sprintf(`{"idx":%d,"kind":"assistant","text":"retry the upload"}`, i))
	}
	file := filepath.Join(r.root, "page.json")
	if err := os.WriteFile(file, []byte("["+strings.Join(rows, ",")+"]"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := func(deflate bool) (int64, []byte) {
		var n atomic.Int64
		c, err := r.dial(t, r.pair(t), r.pin, func(cfg *hostclient.Config) {
			cfg.Deflate = deflate
			cfg.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
				return counted{conn, &n}, err
			}
		})
		if err != nil {
			t.Fatal(err)
		}
		if c.Deflating() != deflate {
			t.Fatalf("asked for deflate %v, got %v", deflate, c.Deflating())
		}
		before := n.Load()
		var got struct {
			Content []byte `json:"content"`
		}
		if err := call(t, c, "fs.read", map[string]string{"path": file}, &got); err != nil {
			t.Fatal(err)
		}
		return n.Load() - before, got.Content
	}
	plainBytes, plain := read(false)
	packedBytes, packed := read(true)
	if !bytes.Equal(packed, plain) || !bytes.Contains(packed, []byte(`"idx":1999`)) {
		t.Fatal("the compressed page didn't read back as the plain one")
	}
	t.Logf("page: %d bytes compressed, %d plain", packedBytes, plainBytes)
	if packedBytes*3 > plainBytes {
		t.Fatalf("the page took %d bytes compressed and %d plain; want at least 3x smaller", packedBytes, plainBytes)
	}
}

// A stranger admitted to pair has a short window for its one pair.complete,
// separate from the code's own lifetime: one that connects and goes silent is
// dropped rather than holding a slot.
func TestASilentStrangerIsDroppedAfterThePairingDeadline(t *testing.T) {
	wsserver.ShortenTimeouts(t, 5*time.Second, 200*time.Millisecond, 64)
	r := newRemote(t)
	if _, err := r.pairer.Begin("wss://relay.test/ws"); err != nil {
		t.Fatal(err)
	}
	stranger, _ := device.Generate()
	c, err := r.dial(t, stranger, r.pin)
	if err != nil {
		t.Fatal(err)
	}
	testwait.For(t, "the silent stranger dropped", func() bool {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		return c.Ping(ctx) != nil
	})
}

// A passthrough connection that never says hello is closed after the hello
// window, and only so many may wait at once; a paired phone still connects.
func TestUnprovenConnectionsAreCappedAndTimedOut(t *testing.T) {
	wsserver.ShortenTimeouts(t, 300*time.Millisecond, 15*time.Second, 2)
	r := newRemote(t)
	silent := func() (*websocket.Conn, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		ws, _, err := websocket.Dial(ctx, r.url, &websocket.DialOptions{HTTPClient: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13},
		}}})
		if err == nil {
			t.Cleanup(func() { ws.CloseNow() })
		}
		return ws, err
	}
	first, err := silent()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := silent(); err != nil {
		t.Fatal(err)
	}
	if _, err := silent(); err == nil {
		t.Fatal("a third connection waited past the cap")
	}
	// Each silent one is closed at the end of its window, freeing the cap.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		if _, _, err := first.Read(ctx); err != nil {
			break
		}
	}
	if ctx.Err() != nil {
		t.Fatal("a silent connection outlived its hello window")
	}
	phone := r.pair(t)
	testwait.For(t, "a paired phone to connect", func() bool {
		c, err := r.dial(t, phone, r.pin)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
}
