package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/hostupdate"
	"github.com/repogo/host/internal/release"
)

// restarter is how run learns where it was started from and asks to be
// started again, into the binary an update put in its place.
type restarter struct {
	binary string
	marker string
	// failed is why the last update was rolled back, for host.status.
	failed  string
	restart func()
}

// serve runs the host and, when an update asks, execs the new binary once
// run has shut everything down, so no agent, shell or action outlives it.
func serve(ctx context.Context, log *slog.Logger, port int, relay string) error {
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	marker, err := apphome.Path("update.json")
	if err != nil {
		return err
	}
	outcome, err := hostupdate.Resume(marker, binary, release.Version)
	if err != nil {
		log.Error("update: settling the last update failed", "err", err)
	}
	if outcome.RolledBack {
		log.Error("update: new release kept failing to start; rolling back")
		return hostupdate.Exec(binary)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var restart atomic.Bool
	err = run(ctx, log, port, relay, restarter{
		binary: binary, marker: marker, failed: outcome.Failed,
		restart: func() { restart.Store(true); cancel() },
	})
	if err != nil || !restart.Load() {
		return err
	}
	log.Info("update: restarting into the new release")
	return hostupdate.Exec(binary)
}

func update(ctx context.Context, now bool) error {
	var result hostupdate.Result
	if err := callWithin(ctx, 5*time.Minute, "host.update", map[string]bool{"now": now}, &result); err != nil {
		return err
	}
	switch {
	case result.To == "":
		fmt.Println("repogo is up to date (" + result.From + ").")
		return nil
	case result.Busy != nil:
		return fmt.Errorf("%s is available, but updating would stop %s; run update --now to stop them",
			result.To, busyText(*result.Busy))
	}
	fmt.Println("Updating repogo " + result.From + " to " + result.To + "…")
	if err := waitReady(ctx, result.To); err != nil {
		return fmt.Errorf("%w; run repogo logs", err)
	}
	fmt.Println("Updated repogo to " + result.To + ".")
	return nil
}

func busyText(b hostupdate.Busy) string {
	var parts []string
	for _, c := range []struct {
		n    int
		noun string
	}{{b.Chats, "chat"}, {b.Terminals, "terminal"}, {b.Actions, "action"}} {
		switch {
		case c.n == 1:
			parts = append(parts, "1 "+c.noun)
		case c.n > 1:
			parts = append(parts, fmt.Sprintf("%d %ss", c.n, c.noun))
		}
	}
	return strings.Join(parts, ", ")
}
