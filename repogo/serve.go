package main

import (
	"cmp"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agents"
	"github.com/repogoapp/host/internal/apphome"
	"github.com/repogoapp/host/internal/binfetch"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/host"
	"github.com/repogoapp/host/internal/hostinfo"
	"github.com/repogoapp/host/internal/hostupdate"
	"github.com/repogoapp/host/internal/nodejs"
	"github.com/repogoapp/host/internal/notify"
	"github.com/repogoapp/host/internal/power"
	"github.com/repogoapp/host/internal/release"
	"github.com/repogoapp/host/internal/tunnel"
)

func run(ctx context.Context, log *slog.Logger, port int, relayFlag *string, self restarter) error {
	// Leaf packages with one or two lines to say log through slog's default.
	slog.SetDefault(log)
	// A CLI installed from a phone lands in ~/.local/bin, which the service's
	// PATH, fixed when it was installed, may not have.
	if err := binfetch.OnPath(); err != nil {
		return err
	}
	go ensureNode(ctx, log)
	confPath, err := confPath()
	if err != nil {
		return err
	}
	conf, err := loadOrCreateConf(confPath)
	if err != nil {
		return err
	}
	if port != 0 {
		conf.Port = port
	}
	cfg, err := hostConfig(log, conf, relayFor(relayFlag, os.Getenv("REPOGO_HRELAY")), self)
	if err != nil {
		return err
	}
	h, err := host.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer h.Close()

	addr, err := h.Listen()
	if err != nil {
		// The port is sticky, so a leftover repogo is the likely cause.
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("port %d is already in use, most likely by a repogo that is still running.\n"+
				"  Find it:  lsof -nP -iTCP:%d -sTCP:LISTEN\n"+
				"  Stop it:  kill $(lsof -t -iTCP:%d -sTCP:LISTEN)\n"+
				"  Or run this one elsewhere:  -port 0", conf.Port, conf.Port, conf.Port)
		}
		return err
	}
	// Publish the resolved port before anything can try to connect.
	conf.Port = addr.Port
	if err := writeConf(confPath, conf); err != nil {
		return err
	}
	h.Start()

	log.Info("runtime listening", "addr", addr.String(), "conf", confPath)
	// Serving is a start that counts: a rollback is only for a release that cannot get here.
	if err := hostupdate.Settle(self.marker, release.Version); err != nil {
		log.Error("update: settling the new release failed", "err", err)
	}
	<-ctx.Done()
	log.Info("shutting down")
	return nil
}

// projectsDir is ~/RepoGo as the disk spells it. A case-insensitive volume
// opens ~/repogo under either name, but agents record the real spelling as a
// chat's cwd, and a project under the other one would match none of its chats.
func projectsDir(home string) string {
	const name = "RepoGo"
	entries, err := os.ReadDir(home)
	if err != nil {
		return filepath.Join(home, name)
	}
	for _, entry := range entries {
		if entry.Name() == name {
			return filepath.Join(home, name)
		}
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.EqualFold(entry.Name(), name) {
			return filepath.Join(home, entry.Name())
		}
	}
	return filepath.Join(home, name)
}

// relayFor is the relay to link to: the -relay flag when given (empty
// disables), else REPOGO_HRELAY ("off" disables), else RepoGo's.
func relayFor(flagged *string, override string) string {
	switch {
	case flagged != nil:
		return *flagged
	case override == "off":
		return ""
	}
	return cmp.Or(override, defaultRelay)
}

// relayRoots trusts the certificates in REPOGO_HRELAY_CA as well as the
// system's, for a relay of one's own with a private certificate.
func relayRoots() (*x509.CertPool, error) {
	path := os.Getenv("REPOGO_HRELAY_CA")
	if path == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("REPOGO_HRELAY_CA: %w", err)
	}
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("REPOGO_HRELAY_CA: no certificate in %s", path)
	}
	return roots, nil
}

// hostConfig is the production host: the user's own state, home and agents.
func hostConfig(log *slog.Logger, conf *localConf, relay string, self restarter) (host.Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return host.Config{}, fmt.Errorf("locate home dir: %w", err)
	}
	state, err := apphome.Dir()
	if err != nil {
		return host.Config{}, err
	}
	attachments, err := agent.AttachmentsDir()
	if err != nil {
		return host.Config{}, err
	}
	roots, err := relayRoots()
	if err != nil {
		return host.Config{}, err
	}
	gateway := cmp.Or(os.Getenv("REPOGO_GATEWAY"), tunnel.DefaultGateway)
	gatewayHost := gateway
	if h, _, err := net.SplitHostPort(gateway); err == nil {
		gatewayHost = h
	}
	return host.Config{
		State: state, Home: home, Attachments: attachments,
		// Visible in the user's home rather than under a dotfile: it holds their source.
		Projects: projectsDir(home),
		Port:     conf.Port, Token: conf.Token, ServerID: conf.ServerID, Relay: relay, RelayRoots: roots,
		Label:   device.Label(),
		Gateway: gateway, GatewayPlaintext: gatewayHost == "localhost" || gatewayHost == "127.0.0.1",
		Update: hostupdate.Deps{
			Version: release.Version, Root: self.root, Marker: self.marker,
			Latest: release.Latest, Stage: release.Stage, Restart: self.restart,
		},
		UpdateFailed: self.failed,
		PluginsSDK:   pluginsSDK(),
		Agents:       agents.New,
		InstallHooks: notify.EnsureHooks,
		Power: func(log *slog.Logger, wanted func() bool, onBattery func(power.Battery)) (host.Keeper, error) {
			k, err := power.New(log, wanted, onBattery)
			if err != nil {
				return nil, err
			}
			return k, nil
		},
		PublicIP: hostinfo.LookupPublicIP,
		Log:      log,
	}, nil
}

// ensureNode gives a machine without a new enough Node the host's own, for npm
// and npx; run in the background so start-up does not wait.
func ensureNode(ctx context.Context, log *slog.Logger) {
	bin, err := nodejs.Ensure(ctx)
	switch {
	case err != nil:
		log.Warn("node: no Node for npm and npx", "err", err)
	case bin != "":
		if err := binfetch.PrependPath(bin); err != nil {
			log.Warn("node: could not put the host's own Node on PATH", "err", err)
			return
		}
		log.Info("node: using the host's own", "bin", bin)
	}
}

// pluginsSDK is the plugins SDK's folder on a development host, from the dev
// scripts; a release runs the runtime it carries instead.
func pluginsSDK() string {
	if !release.IsDev() {
		return ""
	}
	return os.Getenv("REPOGO_PLUGINS_SDK")
}
