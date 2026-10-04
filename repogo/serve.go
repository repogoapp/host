package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents"
	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/binfetch"
	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/host"
	"github.com/repogo/host/internal/hostinfo"
	"github.com/repogo/host/internal/hostupdate"
	"github.com/repogo/host/internal/nodejs"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/power"
	"github.com/repogo/host/internal/release"
	"github.com/repogo/host/internal/tunnel"
)

func run(ctx context.Context, log *slog.Logger, port int, relayURL string, self restarter) error {
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
	cfg, err := hostConfig(log, conf, relayURL, self)
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

// hostConfig is the production host: the user's own state, home and agents.
func hostConfig(log *slog.Logger, conf *localConf, relayURL string, self restarter) (host.Config, error) {
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
	gateway := cmp.Or(os.Getenv("REPOGO_GATEWAY"), tunnel.DefaultGateway)
	gatewayHost, _, _ := net.SplitHostPort(gateway)
	return host.Config{
		State: state, Home: home, Attachments: attachments,
		// Visible in the user's home rather than under a dotfile: it holds their source.
		Projects: projectsDir(home),
		Port:     conf.Port, Token: conf.Token, ServerID: conf.ServerID, Relay: relayURL,
		Label:   device.Label(),
		Gateway: gateway, GatewayPlaintext: gatewayHost == "localhost" || gatewayHost == "127.0.0.1",
		Getenv: os.Getenv,
		Update: hostupdate.Deps{
			Version: release.Version, Binary: self.binary, Marker: self.marker,
			Latest: release.Latest, Stage: release.Stage, Restart: self.restart,
		},
		UpdateFailed: self.failed,
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
