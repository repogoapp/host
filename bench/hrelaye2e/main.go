// Command hrelaye2e checks a deployed passthrough relay end to end: a
// throwaway host links to it, a pinned client reaches that host by name, and a
// push without Apple's evidence is refused by the relay's gate, not dropped.
//
//	go run ./bench/hrelaye2e [-relay h.repogo.app:443]
package main

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
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/passthrough"
	"github.com/repogoapp/host/internal/pushgate"
)

func main() {
	relay := flag.String("relay", "h.repogo.app:443", "the passthrough relay")
	flag.Parse()
	name, _, _ := net.SplitHostPort(*relay)
	if err := run(*relay, name); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
}

func run(relay, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	host, err := device.Generate()
	if err != nil {
		return err
	}
	cert, pin, err := selfSigned()
	if err != nil {
		return err
	}
	l := passthrough.Listen(ctx, &passthrough.Listener{
		RelayAddr: relay, Identity: host, IdleTimeout: time.Minute,
		Log:     slog.New(slog.NewTextHandler(os.Stderr, nil)),
		LinkTLS: &tls.Config{ServerName: name, MinVersion: tls.VersionTLS13},
	})
	defer l.Close()
	groups := make(chan tls.CurveID, 1)
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "hello from host %s", host.ID)
		}),
		ConnState: func(c net.Conn, s http.ConnState) {
			if tc, ok := c.(*tls.Conn); ok && s == http.StateActive {
				select {
				case groups <- tc.ConnectionState().CurveID:
				default:
				}
			}
		},
	}
	go server.Serve(tls.NewListener(l, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}))
	defer server.Close()
	for !l.Linked() {
		select {
		case <-ctx.Done():
			return errors.New("the host never linked")
		case <-time.After(100 * time.Millisecond):
		}
	}
	fmt.Println("host linked:", host.ID)

	target := string(host.ID) + "." + name
	_, port, _ := net.SplitHostPort(relay)
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
		ServerName: target, MinVersion: tls.VersionTLS13,
		// The pin is the only trust: CA checks are off and the key must match.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo) != pin {
				return errors.New("certificate does not match the pin")
			}
			return nil
		},
	}}}
	start := time.Now()
	resp, err := client.Get("https://" + target + ":" + port + "/")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), string(host.ID)) {
		return fmt.Errorf("unexpected body %q", body)
	}
	fmt.Printf("phone reached %s in %v: %q (group %v)\n", target, time.Since(start).Round(time.Millisecond), body, <-groups)
	return pushRefused(ctx, l, host.ID)
}

// pushRefused sends a push whose grant carries no App Attest evidence: a relay
// with APNs answers denied, one without answers unavailable. Apple is never called.
func pushRefused(ctx context.Context, l *passthrough.Listener, host device.ID) error {
	phone, err := device.Generate()
	if err != nil {
		return err
	}
	target := phone.GrantPush(host, device.PushTarget{Service: device.PushAPNs, Token: "00", Environment: pushgate.EnvironmentSandbox, GrantedAt: time.Now().UnixMilli()})
	err = l.Push(ctx, pushgate.PushRequest{Token: target.Token, Environment: target.Environment, Payload: []byte(`{"aps":{}}`),
		Grant: pushgate.PushGrant{Device: phone.ID, PublicKey: phone.Public, Signature: target.Grant, GrantedAt: target.GrantedAt}})
	var rpcErr *jsonrpc.Error
	switch {
	case errors.As(err, &rpcErr) && rpcErr.Code == jsonrpc.CodeDenied:
		fmt.Println("push answered: denied without App Attest, so APNs is on ✓")
		return nil
	case errors.As(err, &rpcErr) && rpcErr.Code == jsonrpc.CodeUnavailable:
		return errors.New("push answered: this relay has no APNs key")
	case err == nil:
		return errors.New("a push without App Attest was accepted")
	}
	return fmt.Errorf("push: %w", err)
}

func selfSigned() (tls.Certificate, [32]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, [32]byte{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "repogo host"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, [32]byte{}, err
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, sha256.Sum256(leaf.RawSubjectPublicKeyInfo), nil
}
