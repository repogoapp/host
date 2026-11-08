package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/repogoapp/host/internal/power"
	"github.com/repogoapp/host/internal/release"
	"github.com/repogoapp/host/internal/service"
)

// table is every command, in the order `repogo help` lists them.
func table() []command {
	return []command{
		{name: "", summary: "Install, start and show a pairing code (first run)", flags: withDirect(withPairHost(nil)), run: setup("")},
		{name: "pair", summary: "Show a pairing code for another device", flags: withPairHost(func(fs *flag.FlagSet) {
			fs.String("reusable", "", "an invite many devices may join with, for days or hours (14d, 36h), or off")
		}), run: runPair},
		{name: "status", summary: "Whether the host is running, and its version", run: simple("status", printStatus)},

		{name: "start", args: "[--lan] [--tailscale=false]", summary: "Start the background host; the network flags are saved", gap: true, flags: withDirect(nil), run: setup("start")},
		{name: "stop", summary: "Stop the background host", run: runStop},
		{name: "restart", summary: "Restart the background host", run: runRestart},
		{name: "logs", summary: "Follow the host's logs", run: runLogs},
		{name: "update", args: "[--now]", summary: "Update to the latest version; --now stops running work first", flags: func(fs *flag.FlagSet) {
			fs.Bool("now", false, "stop running chats, terminals and actions before updating")
		}, run: runUpdate},

		{name: "devices", args: "[revoke <id> | revoke --all [--yes]]", summary: "List paired devices, or remove one or every one", gap: true, flags: func(fs *flag.FlagSet) {
			fs.Bool("all", false, "revoke: every paired device; pair again with repogo pair")
			fs.Bool("yes", false, "revoke --all: don't ask first")
		}, run: devicesCommand},
		{name: "tunnels", args: "[close <slug>]", summary: "List open public tunnels, or close one", run: tunnelsCommand},
		{name: "urls", args: "[<plugin>]", summary: "List the plugin methods that answer plain HTTP and their URLs", run: urlsCommand},
		{name: "keys", args: "[<plugin> [--new | --delete]]", summary: "Show a plugin's key for its HTTP methods, making it if needed", flags: func(fs *flag.FlagSet) {
			fs.Bool("new", false, "replace the plugin's key; the old one stops working")
			fs.Bool("delete", false, "remove the plugin's key; its methods that take one answer no one")
		}, run: keysCommand},
		{name: "tls", args: "[rotate [--yes]]", summary: "Show the TLS key phones pin, or make a new one", flags: withYes, run: tlsCommand},
		{name: "keep-awake", args: "[status|enable|disable]", summary: "Keep a Mac awake with its lid closed", run: keepAwakeCommand},

		{name: "install", args: "[--lan] [--tailscale=false]", summary: "Reinstall the background service and start it", gap: true, flags: withDirect(nil), run: setup("install")},
		{name: "uninstall", args: "[--yes]", summary: "Remove the service and agent hooks; keeps ~/.repogo", flags: withYes, run: runUninstall},
		{name: "serve", args: "[--port N] [--relay host:port] [--lan] [-v]", summary: "Run in the foreground (development); flags last this run only", flags: serveFlags, run: runServe},
		{name: "version", summary: "Print the version", run: func(_ context.Context, _ *flag.FlagSet, args []string) error {
			if err := noArgs("version", args); err != nil {
				return err
			}
			fmt.Println(release.Version)
			return nil
		}},
		{name: "help", args: "[command]", summary: "This list, or one command's flags", run: runHelp},
	}
}

func withYes(fs *flag.FlagSet) { fs.Bool("yes", false, "don't ask first") }

func withPairHost(more func(*flag.FlagSet)) func(*flag.FlagSet) {
	return func(fs *flag.FlagSet) {
		fs.String("pair-host", defaultPairHost, "pairing link origin")
		if more != nil {
			more(fs)
		}
	}
}

// withDirect adds --lan and --tailscale, which are saved, so the background
// host keeps them across restarts.
func withDirect(more func(*flag.FlagSet)) func(*flag.FlagSet) {
	return func(fs *flag.FlagSet) {
		fs.Bool("lan", false, "also serve phones on this machine's local network (--lan=false turns it off)")
		fs.Bool("tailscale", true, "serve phones on this machine's Tailscale address (--tailscale=false turns it off)")
		if more != nil {
			more(fs)
		}
	}
}

func flagString(fs *flag.FlagSet, name string) string { return fs.Lookup(name).Value.String() }
func flagBool(fs *flag.FlagSet, name string) bool {
	return fs.Lookup(name) != nil && fs.Lookup(name).Value.String() == "true"
}

// setFlags is which flags the command line named, not just their values.
func setFlags(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

func simple(name string, fn func(context.Context) error) func(context.Context, *flag.FlagSet, []string) error {
	return func(ctx context.Context, _ *flag.FlagSet, args []string) error {
		if err := noArgs(name, args); err != nil {
			return err
		}
		return fn(ctx)
	}
}

func runHelp(_ context.Context, _ *flag.FlagSet, args []string) error {
	if len(args) == 0 {
		printHelp(os.Stdout)
		return nil
	}
	c, ok := find(args[0])
	if !ok || len(args) > 1 {
		return fmt.Errorf("no command %q; run repogo help", strings.Join(args, " "))
	}
	printCommand(os.Stdout, c)
	return nil
}

// setup installs the service when it isn't running this binary, starts it,
// then answers as the command does: bare and install show the status, or a
// pairing code when nothing is paired; start says it is running.
func setup(name string) func(context.Context, *flag.FlagSet, []string) error {
	return func(ctx context.Context, fs *flag.FlagSet, args []string) error {
		if err := noArgs(name, args); err != nil {
			return err
		}
		directChanged, err := saveDirectFlags(fs)
		if err != nil {
			return err
		}
		if err := installAndStart(ctx, name == "install", directChanged); err != nil {
			return err
		}
		if name == "start" {
			noteOlderHost(ctx)
			fmt.Println("Host is running in the background.")
			return nil
		}
		return pairOrStatus(ctx, false, pairHostOf(fs))
	}
}

func pairHostOf(fs *flag.FlagSet) string {
	if fs.Lookup("pair-host") == nil {
		return defaultPairHost
	}
	return flagString(fs, "pair-host")
}

// saveDirectFlags saves --lan and --tailscale when the command line named
// them, reporting whether either changed.
func saveDirectFlags(fs *flag.FlagSet) (bool, error) {
	set := setFlags(fs)
	var lan, tailscale *bool
	if set["lan"] {
		v := flagBool(fs, "lan")
		lan = &v
	}
	if set["tailscale"] {
		v := flagBool(fs, "tailscale")
		tailscale = &v
	}
	if lan == nil && tailscale == nil {
		return false, nil
	}
	return saveDirect(lan, tailscale)
}

// installAndStart installs the service when it doesn't run this binary (or
// always, for install), puts repogo on PATH, and starts it; a running host
// restarts only when a saved network setting changed.
func installAndStart(ctx context.Context, reinstall, directChanged bool) error {
	s, err := service.New()
	if err != nil {
		return err
	}
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
	if !current || reinstall {
		if installed {
			if err := s.Stop(ctx); err != nil {
				return err
			}
		}
		if err := s.Install(ctx, binary); err != nil {
			return err
		}
		if err := service.Linger(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
	}
	// Every setup, so a host installed before the link gets one too.
	linkOnPath(os.Stdout)
	start := s.Start
	if directChanged && current && !reinstall {
		start = s.Restart
	}
	if err := start(ctx); err != nil {
		return err
	}
	if err := waitReady(ctx, ""); err != nil {
		return fmt.Errorf("%w; run repogo logs", err)
	}
	return nil
}

func runPair(ctx context.Context, fs *flag.FlagSet, args []string) error {
	if err := noArgs("pair", args); err != nil {
		return err
	}
	if reusable := flagString(fs, "reusable"); reusable != "" {
		return pairReusable(ctx, reusable, pairHostOf(fs))
	}
	// Not running yet: set it up first, as a bare repogo does.
	if err := call(ctx, "host.status", nil, nil); err != nil {
		if err := installAndStart(ctx, false, false); err != nil {
			return err
		}
	}
	return pairOrStatus(ctx, true, pairHostOf(fs))
}

func runStop(ctx context.Context, _ *flag.FlagSet, args []string) error {
	if err := noArgs("stop", args); err != nil {
		return err
	}
	s, err := service.New()
	if err != nil {
		return err
	}
	if err := s.Stop(ctx); err != nil {
		return err
	}
	fmt.Println("Host stopped. Phones can't reach it until repogo start.")
	return nil
}

func runRestart(ctx context.Context, _ *flag.FlagSet, args []string) error {
	if err := noArgs("restart", args); err != nil {
		return err
	}
	s, err := service.New()
	if err != nil {
		return err
	}
	if err := s.Restart(ctx); err != nil {
		return err
	}
	if err := waitReady(ctx, ""); err != nil {
		return fmt.Errorf("%w; run repogo logs", err)
	}
	fmt.Println("Host restarted.")
	return nil
}

func runLogs(_ context.Context, _ *flag.FlagSet, args []string) error {
	if err := noArgs("logs", args); err != nil {
		return err
	}
	s, err := service.New()
	if err != nil {
		return err
	}
	return s.Logs()
}

func runUpdate(ctx context.Context, fs *flag.FlagSet, args []string) error {
	if err := noArgs("update", args); err != nil {
		return err
	}
	return update(ctx, flagBool(fs, "now"))
}

func runUninstall(ctx context.Context, fs *flag.FlagSet, args []string) error {
	if err := noArgs("uninstall", args); err != nil {
		return err
	}
	if err := confirm("Remove the background service and agent hooks? Pairings and chats stay in ~/.repogo.", flagBool(fs, "yes")); err != nil {
		return err
	}
	s, err := service.New()
	if err != nil {
		return err
	}
	if err := s.Uninstall(ctx); err != nil {
		return err
	}
	if err := removeHooks(); err != nil {
		return err
	}
	unlinkFromPath()
	fmt.Println("Background service and agent hooks removed. Pairings and chat data are kept in ~/.repogo.")
	if power.Enabled() {
		fmt.Println("The lid-hold sudoers rule stays; remove it with repogo keep-awake disable.")
	}
	return nil
}
