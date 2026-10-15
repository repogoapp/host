package main

import (
	"cmp"
	"context"
	"fmt"
	"net/url"

	"github.com/repogoapp/host/internal/apphome"
	"github.com/repogoapp/host/internal/service"
)

// relayCommand shows or changes the relay phones reach this host through.
// Notifications go through RepoGo's relay either way: it holds the APNs key.
func relayCommand(ctx context.Context, args []string) error {
	switch {
	case len(args) == 0:
		saved, err := savedRelay()
		if err != nil {
			return err
		}
		if saved == "" {
			fmt.Println("Relay: " + defaultRelay + " (RepoGo's)")
			return nil
		}
		fmt.Println("Relay: " + saved + " (your own; notifications still go through RepoGo)")
		return nil
	case len(args) == 2 && args[0] == "set":
		if err := checkRelay(args[1]); err != nil {
			return err
		}
		return switchRelay(ctx, args[1])
	case len(args) == 1 && args[0] == "reset":
		return switchRelay(ctx, "")
	default:
		return fmt.Errorf("usage: repogo relay [set <wss-url>|reset]")
	}
}

// checkRelay accepts a WebSocket URL with a host, the only thing a phone can dial.
func checkRelay(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return fmt.Errorf("%q is not a relay URL; it looks like wss://relay.example.com/ws", raw)
	}
	return nil
}

// switchRelay saves relay ("" for RepoGo's) and restarts the host on it. Paired
// devices are revoked first, while the old relay can still tell them: their
// saved address stops working, so they pair again.
func switchRelay(ctx context.Context, relay string) error {
	saved, err := savedRelay()
	if err != nil {
		return err
	}
	if cmp.Or(saved, defaultRelay) == cmp.Or(relay, defaultRelay) {
		fmt.Println("The host already uses that relay.")
		return nil
	}
	paired, err := peers(ctx)
	if err != nil {
		return fmt.Errorf("%w; the host must be running to change its relay", err)
	}
	for _, d := range paired {
		if err := call(ctx, "devices.revoke", map[string]string{"id": string(d.ID)}, nil); err != nil {
			return err
		}
	}
	if err := saveRelay(relay); err != nil {
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
	fmt.Printf("Relay is now %s. %d paired device(s) removed; run repogo pair to pair again.\n",
		cmp.Or(relay, defaultRelay), len(paired))
	return nil
}

// savedRelay is the relay `repogo relay set` saved, or "" for RepoGo's.
func savedRelay() (string, error) {
	path, err := confPath()
	if err != nil {
		return "", err
	}
	var c localConf
	if _, err := apphome.ReadJSON(path, &c); err != nil {
		return "", err
	}
	return c.Relay, nil
}

func saveRelay(relay string) error {
	path, err := confPath()
	if err != nil {
		return err
	}
	c, err := loadOrCreateConf(path)
	if err != nil {
		return err
	}
	c.Relay = relay
	return writeConf(path, c)
}
