// Command flysni checks plan 520's Fly gate: raw TCP on 443 reaches the app
// with the TLS ClientHello untouched, so TLS can end on a host, not at Fly.
// It ends TLS itself with a throwaway cert and reports what it saw.
package main

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"log"
	"math/big"
	"net"
	"strings"
	"time"
)

func main() {
	cert := selfSigned()
	// :8443 takes raw TCP; :8444 takes the same behind a PROXY v1 header.
	go serve(":8443", cert, false)
	serve(":8444", cert, true)
}

func serve(addr string, cert tls.Certificate, proxied bool) {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("listening on %s (proxy header: %v)", addr, proxied)
	for {
		c, err := lis.Accept()
		if err != nil {
			log.Print(err)
			continue
		}
		go handle(c, cert, proxied)
	}
}

func handle(raw net.Conn, cert tls.Certificate, proxied bool) {
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(15 * time.Second))
	reader := bufio.NewReader(raw)
	client := raw.RemoteAddr().String()
	if proxied {
		line, err := reader.ReadString('\n')
		if err != nil || !strings.HasPrefix(line, "PROXY ") {
			log.Printf("no PROXY header: %q %v", line, err)
			return
		}
		if f := strings.Fields(line); len(f) >= 6 {
			client = net.JoinHostPort(f[2], f[4])
		}
	}
	var sni string
	conn := tls.Server(&buffered{raw, reader}, &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			sni = hello.ServerName
			return &cert, nil
		},
	})
	if err := conn.Handshake(); err != nil {
		log.Printf("handshake from %s (sni %q): %v", client, sni, err)
		return
	}
	state := conn.ConnectionState()
	body := fmt.Sprintf("flysni: tls ended in the app\nsni=%s\nclient=%s\nfly_peer=%s\nproxy_header=%v\nversion=%s\ngroup=%s\n",
		sni, client, raw.RemoteAddr(), proxied, tls.VersionName(state.Version), state.CurveID)
	log.Print(strings.ReplaceAll(body, "\n", " "))
	fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
}

// buffered keeps bytes the PROXY header read ahead of the ClientHello.
type buffered struct {
	net.Conn
	r *bufio.Reader
}

func (b *buffered) Read(p []byte) (int, error) { return b.r.Read(p) }

func selfSigned() tls.Certificate {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "flysni probe"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(30 * 24 * time.Hour),
		DNSNames:     []string{"*.h.repogo.app"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
