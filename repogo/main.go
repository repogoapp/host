// Command repogo installs, pairs, and runs the host.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/agents"
	"github.com/repogoapp/host/internal/devlog"
	"github.com/repogoapp/host/internal/notify"
)

// defaultRelay is RepoGo's relay: phones reach the host through it over the
// host's pinned TLS, and pushes go out through it.
const defaultRelay = "h.repogo.app:443"
const defaultPairHost = "https://repogo.app"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := dispatch(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, errDeclined) {
			fmt.Println("Left as it was.")
			return
		}
		fmt.Fprintln(os.Stderr, "repogo:", err)
		os.Exit(1)
	}
}

func serveFlags(fs *flag.FlagSet) {
	fs.Int("port", 0, "loopback port (0 uses the saved port)")
	fs.String("relay", "", "relay address, host:port (default: RepoGo's; empty disables)")
	fs.Bool("lan", false, "also serve the local network for this run; repogo start --lan saves it")
	fs.Bool("v", false, "debug logging")
}

// runServe is the host in the foreground: what the background service runs,
// and what development runs by hand.
func runServe(ctx context.Context, fs *flag.FlagSet, args []string) error {
	if err := noArgs("serve", args); err != nil {
		return err
	}
	level := slog.LevelInfo
	if flagBool(fs, "v") {
		level = slog.LevelDebug
	}
	var handler slog.Handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	// Dev only: restart.sh names the log server. A release host leaves it
	// unset and logs to stderr alone.
	if base := os.Getenv(devlog.EnvVar); base != "" {
		handler = devlog.Tee(ctx, handler, base)
	}
	log := slog.New(handler)
	if addr := os.Getenv(pprofEnvVar); addr != "" {
		if err := servePprof(log, addr); err != nil {
			return err
		}
	}
	// Unset, the relay is REPOGO_HRELAY's, else RepoGo's.
	var relayFlag *string
	if setFlags(fs)["relay"] {
		relay := flagString(fs, "relay")
		relayFlag = &relay
	}
	port := 0
	fmt.Sscan(flagString(fs, "port"), &port)
	return serve(ctx, log, port, flagBool(fs, "lan"), relayFlag)
}

// removeHooks takes the hook helper out of every agent's config, leaving the
// other hooks there as they were.
func removeHooks() error {
	r, err := agents.New(agent.Dependencies{})
	if err != nil {
		return err
	}
	defer r.Close()
	return notify.RemoveHooks(r.Hooks)
}
