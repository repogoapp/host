// Command relay is the hosted byte router: no agents, no filesystem, no database.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/repogo/host/internal/appattest"
	"github.com/repogo/host/internal/relay"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))

	// Fly injects PORT and expects the process to honour it.
	addr := ":" + env("PORT", "8080")
	verifier, err := appattest.New(env("APPLE_APP_ID_PREFIX", os.Getenv("APNS_TEAM_ID")) + "." + env("APPLE_BUNDLE_ID", "app.repogo"))
	push := apnsPush(log)
	if err != nil && push != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
	var verify func([]byte, []byte, []byte, []byte, string, time.Time) error
	if verifier != nil {
		verify = verifier.Verify
	}

	srv, err := relay.New(relay.Config{
		Addr: addr,
		// Stable per deployment: a signature made for one relay must not be
		// replayable against another. Fly's app name is exactly that.
		ServerID:   env("RELAY_SERVER_ID", env("FLY_APP_NAME", "repogo-relay-dev")),
		Push:       push,
		VerifyPush: verify,
		// Only a proxy that sets this header on every request may name it, or callers forge it.
		ClientIPHeader: os.Getenv("RELAY_CLIENT_IP_HEADER"),
		Log:            log,
	})
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}

	listenAddr, err := srv.Listen()
	if err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
	log.Info("relay listening", "addr", listenAddr.String(), "version", relay.Version())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("shutdown", "err", err)
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func logLevel() slog.Level {
	if os.Getenv("RELAY_DEBUG") != "" {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}
