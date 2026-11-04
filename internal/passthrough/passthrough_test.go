package passthrough

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/testwait"
)

const (
	suffix   = ".h.test"
	linkName = "h.test"
	secret   = "plaintext only the host and phone see"
)

type stack struct {
	relay     *Relay
	listener  *Listener
	direct    string
	relayAddr string
	tapped    *tap
	host      *device.Identity
	pin       [32]byte
}

// start runs a relay, a sniffing forwarder in front of it, and a host serving
// HTTP over the pinned TLS behind its Listener.
func start(t *testing.T) *stack { return startWith(t, options{}) }

// options shape a stack: relay settings, and the addresses a PROXY-speaking
// front (Fly's proxy) reports for the phone's and the host's connections.
type options struct {
	relay           func(*Relay)
	phoneIP, hostIP string
	idleTimeout     time.Duration
}

func startWith(t *testing.T, o options) *stack {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	log := slog.New(slog.DiscardHandler)

	relayCert, relayPool := selfSigned(t, linkName)
	relay := &Relay{
		Suffix: suffix, LinkName: linkName, DialTimeout: 5 * time.Second, MaxPerIP: 64, Log: log,
		LinkTLS: &tls.Config{Certificates: []tls.Certificate{relayCert}, MinVersion: tls.VersionTLS13},
	}
	if o.relay != nil {
		o.relay(relay)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go relay.Serve(ctx, lis)
	tapped := startTap(t, ctx, lis.Addr().String(), o.phoneIP)
	hostPath := lis.Addr().String()
	if o.hostIP != "" {
		hostPath = startTap(t, ctx, hostPath, o.hostIP).addr
	}

	host, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	hostCert, _ := selfSigned(t, "repogo host")
	l := Listen(ctx, &Listener{
		RelayAddr: hostPath, Identity: host, Log: log, IdleTimeout: o.idleTimeout,
		LinkTLS: &tls.Config{RootCAs: relayPool, ServerName: linkName, MinVersion: tls.VersionTLS13},
	})
	t.Cleanup(func() { l.Close() })
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, secret)
	})}
	go server.Serve(tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{hostCert}, MinVersion: tls.VersionTLS13}))
	t.Cleanup(func() { server.Close() })
	testwait.For(t, "the host's link", func() bool { return relay.Linked(host.ID) && l.Linked() })

	return &stack{relay: relay, listener: l, direct: lis.Addr().String(), relayAddr: tapped.addr, tapped: tapped, host: host, pin: spki(hostCert)}
}

// phone dials the host by name through the tap, accepting only pin.
func (s *stack) phone(name string, pin [32]byte) (*http.Response, error) {
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", s.relayAddr)
		},
		TLSClientConfig: &tls.Config{
			ServerName: name, MinVersion: tls.VersionTLS13,
			// The pin replaces CA trust: the key from pairing is the only one accepted.
			InsecureSkipVerify: true,
			VerifyConnection: func(cs tls.ConnectionState) error {
				if len(cs.PeerCertificates) == 0 || spki(tls.Certificate{Certificate: [][]byte{cs.PeerCertificates[0].Raw}}) != pin {
					return errors.New("certificate does not match the pin")
				}
				return nil
			},
		},
	}}
	return client.Get("https://" + name + "/")
}

func TestPhoneReachesHostAndRelaySeesOnlyCiphertext(t *testing.T) {
	s := start(t)
	resp, err := s.phone(string(s.host.ID)+suffix, s.pin)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != secret {
		t.Fatalf("body = %q", body)
	}
	if s.tapped.saw(secret) {
		t.Fatal("the plaintext crossed the relay")
	}
}

func TestPhoneRefusesAnotherKey(t *testing.T) {
	s := start(t)
	if _, err := s.phone(string(s.host.ID)+suffix, [32]byte{1}); err == nil {
		t.Fatal("connected with a wrong pin")
	}
}

func TestUnknownHostIsClosed(t *testing.T) {
	s := start(t)
	other, _ := device.Generate()
	if _, err := s.phone(string(other.ID)+suffix, s.pin); err == nil {
		t.Fatal("reached a host that holds no link")
	}
}

func TestAcceptNeedsTheNoticeToken(t *testing.T) {
	s := start(t)
	conn, err := tls.Dial("tcp", s.relayAddr, &tls.Config{ServerName: linkName, InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	json.NewEncoder(conn).Encode(hostLine{Op: "accept", Token: "guessed"})
	var answer relayLine
	if err := json.NewDecoder(conn).Decode(&answer); err != nil || answer.OK {
		t.Fatalf("a guessed token was accepted: %+v %v", answer, err)
	}
}

func TestLinkNeedsTheMatchingKey(t *testing.T) {
	s := start(t)
	impostor, _ := device.Generate()
	conn, err := tls.Dial("tcp", s.relayAddr, &tls.Config{ServerName: linkName, InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
	enc.Encode(hostLine{Op: "link"})
	var challenge relayLine
	if err := dec.Decode(&challenge); err != nil {
		t.Fatal(err)
	}
	msg, _ := device.ChallengeMessage(challenge.Nonce, challenge.ServerID, challenge.WallMS)
	// Claims the real host's id with its own key and signature.
	enc.Encode(hostLine{Op: "hello", DeviceID: string(s.host.ID), PublicKey: impostor.Public, Signature: impostor.Sign(msg)})
	var answer relayLine
	if err := dec.Decode(&answer); err != nil || answer.OK {
		t.Fatalf("an impostor took the host's link: %+v %v", answer, err)
	}
}

func TestPeekKeepsEveryByte(t *testing.T) {
	var hello bytes.Buffer
	// The client writes its hello, then reads EOF and gives up.
	tls.Client(&pipeConn{w: &hello}, &tls.Config{ServerName: "abc" + suffix}).Handshake()
	raw := hello.Bytes()
	name, seen, err := peekServerName(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil || name != "abc"+suffix || !bytes.Equal(seen, raw) {
		t.Fatalf("name %q, %d of %d bytes, %v", name, len(seen), len(raw), err)
	}
}

func TestProxyLineNamesTheCaller(t *testing.T) {
	s := startWith(t, options{relay: func(r *Relay) { r.ProxyProtocol = true }, phoneIP: "203.0.113.7", hostIP: "198.51.100.2"})
	resp, err := s.phone(string(s.host.ID)+suffix, s.pin)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// The relay counts a connection under the address the line names.
	idle, err := net.Dial("tcp", s.relayAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	testwait.For(t, "the phone's address", func() bool { return s.relay.open("203.0.113.7") == 1 })
	// Without the line, a relay that expects one closes the connection.
	conn, err := tls.Dial("tcp", s.direct, &tls.Config{ServerName: linkName, InsecureSkipVerify: true})
	if err == nil {
		conn.Close()
		t.Fatal("a connection with no PROXY line was served")
	}
}

func TestCapLimitsOneAddress(t *testing.T) {
	const phoneIP = "203.0.113.9"
	s := startWith(t, options{
		relay:   func(r *Relay) { r.ProxyProtocol, r.MaxPerIP = true, 2 },
		phoneIP: phoneIP, hostIP: "198.51.100.3",
	})
	// Two idle connections from the phone's address fill its cap.
	for range 2 {
		idle, err := net.Dial("tcp", s.relayAddr)
		if err != nil {
			t.Fatal(err)
		}
		defer idle.Close()
	}
	testwait.For(t, "two connections counted", func() bool { return s.relay.open(phoneIP) == 2 })
	if _, err := s.phone(string(s.host.ID)+suffix, s.pin); err == nil {
		t.Fatal("a third connection from a capped address got through")
	}
	// Another address, and the host itself, are not held to the phone's cap.
	other := startTap(t, t.Context(), s.direct, "203.0.113.10")
	s.relayAddr = other.addr
	resp, err := s.phone(string(s.host.ID)+suffix, s.pin)
	if err != nil {
		t.Fatalf("another address was refused: %v", err)
	}
	resp.Body.Close()
}

func TestPingsKeepAnIdleLink(t *testing.T) {
	s := startWith(t, options{relay: func(r *Relay) { r.PingInterval = 20 * time.Millisecond }, idleTimeout: 200 * time.Millisecond})
	s.relay.mu.Lock()
	first := s.relay.links[s.host.ID]
	s.relay.mu.Unlock()
	testwait.For(t, "ten pings", func() bool { return s.relay.pinged(s.host.ID) >= 10 })
	s.relay.mu.Lock()
	same := s.relay.links[s.host.ID] == first
	s.relay.mu.Unlock()
	if !same || !s.listener.Linked() {
		t.Fatal("the link was dropped though pings kept coming")
	}
}

func TestSilentRelayIsRedialed(t *testing.T) {
	// A relay that accepts the link, then never sends again, not even a ping.
	cert, pool := selfSigned(t, linkName)
	lis, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	var mu sync.Mutex
	links := 0
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				var first, hello hostLine
				readLine(r, &first)
				json.NewEncoder(c).Encode(relayLine{Nonce: make([]byte, device.NonceLen), ServerID: linkName, WallMS: 1})
				readLine(r, &hello)
				json.NewEncoder(c).Encode(relayLine{OK: true})
				mu.Lock()
				links++
				mu.Unlock()
				io.Copy(io.Discard, c)
			}()
		}
	}()
	host, _ := device.Generate()
	l := Listen(t.Context(), &Listener{
		RelayAddr: lis.Addr().String(), Identity: host, Log: slog.New(slog.DiscardHandler), IdleTimeout: 50 * time.Millisecond,
		LinkTLS: &tls.Config{RootCAs: pool, ServerName: linkName},
	})
	t.Cleanup(func() { l.Close() })
	testwait.For(t, "a second link after the first went silent", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return links >= 2
	})
}

// tap forwards to the relay and keeps every byte, as a network observer would.
type tap struct {
	addr string
	mu   sync.Mutex
	all  bytes.Buffer
}

// startTap opens a forwarder; with proxyIP it first sends the PROXY v1 line
// Fly's proxy would, naming proxyIP as the caller.
func startTap(t *testing.T, ctx context.Context, target, proxyIP string) *tap {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tp := &tap{addr: lis.Addr().String()}
	go func() { <-ctx.Done(); lis.Close() }()
	go func() {
		for {
			c, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				up, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer up.Close()
				if proxyIP != "" {
					fmt.Fprintf(up, "PROXY TCP4 %s 10.0.0.1 40000 443\r\n", proxyIP)
				}
				// Either side ending ends both, as a real network path would.
				go func() { io.Copy(io.MultiWriter(c, tp), up); c.Close() }()
				io.Copy(io.MultiWriter(up, tp), c)
			}()
		}
	}()
	return tp
}

func (tp *tap) Write(p []byte) (int, error) {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return tp.all.Write(p)
}

func (tp *tap) saw(s string) bool {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	return bytes.Contains(tp.all.Bytes(), []byte(s))
}

func selfSigned(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
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
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// spki is the pin: SHA-256 of the leaf's public key, not of the certificate.
func spki(c tls.Certificate) [32]byte {
	leaf, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return [32]byte{}
	}
	return sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
}

// pipeConn records what a TLS client writes and answers nothing.
type pipeConn struct {
	readOnly
	w *bytes.Buffer
}

func (p *pipeConn) Write(b []byte) (int, error) { return p.w.Write(b) }
func (p *pipeConn) Read([]byte) (int, error)    { return 0, io.EOF }

// Before anyone signs in, a link's first line is read; one with no end must
// be cut off at maxLine, not buffered until memory runs out.
func TestAnEndlessLineIsCutOff(t *testing.T) {
	s := start(t)
	conn, err := tls.Dial("tcp", s.relayAddr, &tls.Config{ServerName: linkName, InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go conn.Write(bytes.Repeat([]byte("a"), 4*maxLine))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the relay kept reading a %d-byte line: %v", 4*maxLine, err)
	}
}

// A host that never dials back (or one being flooded) holds at most
// MaxWaitingPerHost phone streams; past that the relay closes new ones at once.
func TestWaitingStreamsPerHostAreCapped(t *testing.T) {
	s := startWith(t, options{relay: func(r *Relay) { r.MaxWaitingPerHost, r.DialTimeout = 2, time.Minute }})
	// A second, silent host: linked, but it never answers a dial notice.
	silent, _ := device.Generate()
	link, err := tls.Dial("tcp", s.direct, &tls.Config{ServerName: linkName, InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	defer link.Close()
	r := bufio.NewReader(link)
	json.NewEncoder(link).Encode(hostLine{Op: "link"})
	var challenge relayLine
	if err := readLine(r, &challenge); err != nil {
		t.Fatal(err)
	}
	msg, _ := device.ChallengeMessage(challenge.Nonce, challenge.ServerID, challenge.WallMS)
	json.NewEncoder(link).Encode(hostLine{Op: "hello", DeviceID: string(silent.ID), PublicKey: silent.Public, Signature: silent.Sign(msg)})
	testwait.For(t, "the silent host's link", func() bool { return s.relay.Linked(silent.ID) })

	name := string(silent.ID) + suffix
	dial := func() *tls.Conn {
		c := tls.Client(mustDial(t, s.direct), &tls.Config{ServerName: name, InsecureSkipVerify: true})
		go c.Handshake()
		t.Cleanup(func() { c.Close() })
		return c
	}
	dial()
	dial()
	testwait.For(t, "two streams waiting", func() bool { return s.relay.waitingOn(silent.ID) == 2 })
	third := dial()
	third.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := third.Read(make([]byte, 1)); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("a third stream waited past the cap")
	}
	// The cap is per host: the real host still answers.
	resp, err := s.phone(string(s.host.ID)+suffix, s.pin)
	if err != nil {
		t.Fatalf("another host was refused: %v", err)
	}
	resp.Body.Close()
}

func mustDial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
