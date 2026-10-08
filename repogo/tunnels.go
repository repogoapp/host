package main

import (
	"context"
	"fmt"
	"time"

	"github.com/repogo/host/internal/tunnel"
)

// tunnelsCommand lists the public URLs this host serves, or closes one.
func tunnelsCommand(ctx context.Context, args []string) error {
	switch {
	case len(args) == 0:
		var status tunnel.Changed
		if err := call(ctx, "tunnels.list", nil, &status); err != nil {
			return err
		}
		if len(status.Tunnels) == 0 {
			fmt.Println("No tunnels open. Open one from the Ports sheet in RepoGo.")
			return nil
		}
		fmt.Printf("Gateway connected: %t\n", status.Connected)
		for _, t := range status.Tunnels {
			fmt.Printf("  %s → 127.0.0.1:%d  (%s, until %s)\n", t.URL, t.Port, t.Slug,
				time.UnixMilli(t.ExpiresAt).Local().Format("Jan 2 15:04"))
		}
		return nil
	case len(args) == 2 && args[0] == "close":
		if err := call(ctx, "tunnels.close", map[string]string{"slug": args[1]}, nil); err != nil {
			return err
		}
		fmt.Println("Closed. The URL stops working now.")
		return nil
	default:
		return fmt.Errorf("usage: repogo tunnels [close <slug>]")
	}
}

// accountCommand releases this host from the RepoGo account that claimed it.
func accountCommand(ctx context.Context, args []string) error {
	if len(args) != 1 || args[0] != "release" {
		return fmt.Errorf("usage: repogo account release")
	}
	if err := call(ctx, "host.release", nil, nil); err != nil {
		return err
	}
	fmt.Println("Released. Another RepoGo account can claim this computer once the first releases it in the app.")
	return nil
}
