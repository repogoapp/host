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
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/repogoapp/host/internal/appattest"
	"github.com/repogoapp/host/internal/device"
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
		Limits:            hostLimits,
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
// pushGate answers hosts' pushes with the keys from the environment: APNs for
// iOS, FCM for Android, whose grants pass Play Integrity or come from a device
// in FCM_TRUSTED_DEVICES (test phones on builds Play didn't install, by their
// device ids, comma-separated). With no key at all it is nil and the relay
// refuses pushes, as a local run does.
func pushGate(log *slog.Logger) (*pushgate.Gate, error) {
	apns, fcm := pushgate.APNs(log), pushgate.FCM(log)
	if apns == nil && fcm == nil {
		log.Warn("hrelay: no APNs or FCM key; pushes are refused")
		return nil, nil
	}
	var verify pushgate.VerifyApp
	if apns != nil {
		verifier, err := appattest.New(pushgate.AppID())
		if err != nil {
			return nil, err
		}
		verify = verifier.Verify
	}
	var trusted []device.ID
	for _, id := range strings.Split(os.Getenv("FCM_TRUSTED_DEVICES"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			trusted = append(trusted, device.ID(id))
		}
	}
	integrity := pushgate.PlayIntegrity(log)
	log.Info("hrelay: pushes on", "bundle", pushgate.BundleID(), "apns", apns != nil, "fcm", fcm != nil,
		"play_integrity", integrity != nil, "trusted_devices", len(trusted))
	return pushgate.New(apns, verify).WithFCM(fcm, integrity, trusted), nil
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

// hostLimits meter each environment's phones together. The heaviest week
// measured (11-05) peaked at 285 MB a day and 11 MB a minute, so ordinary use
// never meets them; a host used as a bulk tunnel is slowed, then cut off.
var hostLimits = passthrough.Limits{
	Burst:     32 << 20,
	Rate:      4 << 20,
	DailyWarn: 2 << 30,
	DailyCap:  10 << 30,
	Idle:      2 * time.Minute,
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
