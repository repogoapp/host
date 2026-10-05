// Command repogo installs, pairs, and runs the host.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents"
	"github.com/repogo/host/internal/devlog"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/power"
	"github.com/repogo/host/internal/release"
	"github.com/repogo/host/internal/service"
)

const defaultRelay = "wss://relay.repogo.app/ws"
const defaultPairHost = "https://repogo.app"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := command(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "repogo:", err)
		os.Exit(1)
	}
}

func command(ctx context.Context, args []string) error {
	verb := ""
	if len(args) > 0 {
		verb, args = args[0], args[1:]
	}
	if verb == "version" || verb == "--version" {
		fmt.Println(release.Version)
		return nil
	}
	if verb == "help" || verb == "--help" || verb == "-h" {
		fmt.Println("repogo [install|pair [--reusable 14d|off]|status|start|stop|restart|logs|uninstall|update [--now]|version]\nrepogo power [status|enable|disable]\nrepogo tunnels [close <slug>]\nrepogo account release\nrepogo serve [-port N] [-relay URL] [-v]\nrepogo invite")
		return nil
	}
	if verb == "power" {
		return powerCommand(ctx, args)
	}
	if verb == "tunnels" {
		return tunnelsCommand(ctx, args)
	}
	if verb == "account" {
		return accountCommand(ctx, args)
	}
	if verb == "invite" {
		if len(args) != 0 {
			return fmt.Errorf("unexpected arguments: %v", args)
		}
		return printInvite(ctx, os.Stdout)
	}
	flags := flag.NewFlagSet("repogo "+verb, flag.ContinueOnError)
	if verb == "serve" {
		port := flags.Int("port", 0, "loopback port (0 uses the saved port)")
		relay := flags.String("relay", defaultRelay, "relay URL (empty disables)")
		verbose := flags.Bool("v", false, "debug logging")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("unexpected arguments: %v", flags.Args())
		}
		level := slog.LevelInfo
		if *verbose {
			level = slog.LevelDebug
		}
		var handler slog.Handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
		// Dev only: restart.sh names the log server. A release host leaves it
		// unset and logs to stderr alone.
		if base := os.Getenv(devlog.EnvVar); base != "" {
			handler = devlog.Tee(ctx, handler, base)
		}
		return serve(ctx, slog.New(handler), *port, *relay)
	}
	now := flags.Bool("now", false, "stop running chats, terminals and actions before updating")
	pairHost := flags.String("pair-host", defaultPairHost, "pairing link origin")
	reusable := flags.String("reusable", "", "pair: an invite many devices may join with, for days or hours (14d, 36h), or off")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	if *reusable != "" {
		if verb != "pair" {
			return fmt.Errorf("--reusable goes with repogo pair")
		}
		return pairReusable(ctx, *reusable, *pairHost)
	}
	s, err := service.New()
	if err != nil {
		return err
	}
	switch verb {
	case "pair":
		if err := call(ctx, "host.status", nil, nil); err == nil {
			return pairOrStatus(ctx, true, *pairHost)
		}
		fallthrough
	case "", "install", "start":
		installed, err := s.Installed()
		if err != nil {
			return err
		}
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		current, err := s.Runs(binary)
		if err != nil {
			return err
		}
		if !current || verb == "install" {
			if installed {
				if err := s.Stop(ctx); err != nil {
					return err
				}
			}
			if err := s.Install(ctx, binary); err != nil {
				return err
			}
		}
		if err := s.Start(ctx); err != nil {
			return err
		}
		if err := waitReady(ctx, ""); err != nil {
			return fmt.Errorf("%w; run repogo logs", err)
		}
		if verb == "start" {
			noteOlderHost(ctx)
			fmt.Println("Host is running in the background.")
			return nil
		}
		return pairOrStatus(ctx, verb == "pair", *pairHost)
	case "status":
		return printStatus(ctx)
	case "stop":
		return s.Stop(ctx)
	case "restart":
		if err := s.Restart(ctx); err != nil {
			return err
		}
		return waitReady(ctx, "")
	case "logs":
		return s.Logs()
	case "uninstall":
		if err := s.Uninstall(ctx); err != nil {
			return err
		}
		if err := removeHooks(); err != nil {
			return err
		}
		fmt.Println("Background service and agent hooks removed. Pairings and chat data are kept.")
		if power.Enabled() {
			fmt.Println("The lid-hold sudoers rule stays; remove it with repogo power disable.")
		}
		return nil
	case "update":
		return update(ctx, *now)
	default:
		return fmt.Errorf("unknown command %q; run repogo help", verb)
	}
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
