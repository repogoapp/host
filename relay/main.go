// Command relay is the hosted byte router: no agents, no filesystem, no database.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/repogo/host/internal/relay"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel()}))

	// Fly injects PORT and expects the process to honour it.
	addr := ":" + env("PORT", "8080")

	srv, err := relay.New(relay.Config{
		Addr: addr,
		// Stable per deployment: a signature made for one relay must not be
		// replayable against another. Fly's app name is exactly that.
		ServerID: env("RELAY_SERVER_ID", env("FLY_APP_NAME", "repogo-relay-dev")),
		Push:     apnsPush(log),
		// Fly's proxy terminates every connection; this is the real caller.
		ClientIPHeader: "Fly-Client-IP",
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
