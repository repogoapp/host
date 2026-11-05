// Command hrelay is RepoGo's relay: phones reach hosts by name under
// *.h.repogo.app and it only pipes their TLS. It ends TLS only for its own
// name, h.repogo.app, with a Let's Encrypt certificate, for host links, which
// also carry the hosts' pushes to APNs.
package main

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/repogoapp/host/internal/appattest"
	"github.com/repogoapp/host/internal/passthrough"
	"github.com/repogoapp/host/internal/pushgate"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	name := env("HRELAY_NAME", "h.repogo.app")
	linkTLS, err := linkTLS(name, log)
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
	maxPerIP, err := strconv.Atoi(env("HRELAY_MAX_PER_IP", "32"))
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
	gate, err := pushGate(log)
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
	relay := &passthrough.Relay{
		Suffix:   "." + name,
		LinkName: name,
		LinkTLS:  linkTLS,
		// Fly's proxy_proto handler sends the caller's address; a relay run by
		// hand without a proxy in front turns it off.
		ProxyProtocol: env("HRELAY_PROXY_PROTOCOL", "1") == "1",
		MaxPerIP:      maxPerIP,
		// A phone opens one stream per connect; a burst of reconnects from a
		// few phones stays well under this.
		MaxWaitingPerHost: 16,
		DialTimeout:       10 * time.Second,
		PingInterval:      25 * time.Second,
		Push:              gate,
		Log:               log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	addr := env("HRELAY_ADDR", ":8443")
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
	log.Info("hrelay listening", "addr", addr, "name", name, "proxy_protocol", relay.ProxyProtocol)
	if err := relay.Serve(ctx, lis); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// pushGate answers hosts' pushes with the APNs key from the environment; with
// none it is nil and the relay refuses them, as a local run does.
func pushGate(log *slog.Logger) (*pushgate.Gate, error) {
	deliver := pushgate.APNs(log)
	if deliver == nil {
		log.Warn("hrelay: no APNs key; pushes are refused")
		return nil, nil
	}
	verifier, err := appattest.New(pushgate.AppID())
	if err != nil {
		return nil, err
	}
	log.Info("hrelay: pushes on", "bundle", pushgate.BundleID())
	return pushgate.New(deliver, verifier.Verify), nil
}

// linkTLS is the certificate for the relay's own name: a pair from files when
// given (local runs), else Let's Encrypt over TLS-ALPN-01 on this same port.
func linkTLS(name string, log *slog.Logger) (*tls.Config, error) {
	if certFile := os.Getenv("HRELAY_CERT_FILE"); certFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, os.Getenv("HRELAY_KEY_FILE"))
		if err != nil {
			return nil, err
		}
		return &tls.Config{Certificates: []tls.Certificate{cert}}, nil
	}
	// The cache is the Fly volume: certificates outlive deploys, which keeps
	// renewals inside Let's Encrypt's weekly limits.
	dir := env("HRELAY_ACME_DIR", "/data/acme")
	log.Info("hrelay certificate from Let's Encrypt", "name", name, "cache", dir)
	m := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(name),
		Cache:      autocert.DirCache(dir),
		Email:      os.Getenv("HRELAY_ACME_EMAIL"),
	}
	return m.TLSConfig(), nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
