package main

import (
	"context"
	"fmt"
	"time"
)

// devicesCommand lists the paired devices, or revokes one: its open
// connections close and it can no longer reach this host.
func devicesCommand(ctx context.Context, args []string) error {
	switch {
	case len(args) == 0:
		paired, err := peers(ctx)
		if err != nil {
			return err
		}
		if len(paired) == 0 {
			fmt.Println("No devices paired. Run repogo pair to add one.")
			return nil
		}
		for _, d := range paired {
			fmt.Printf("  %s  %s (%s, paired %s, connected: %t)\n", d.ID, d.Label, d.Platform,
				time.UnixMilli(d.AddedAt).Local().Format("Jan 2 2006"), d.Connected)
		}
		return nil
	case len(args) == 2 && args[0] == "revoke":
		if err := call(ctx, "devices.revoke", map[string]string{"id": args[1]}, nil); err != nil {
			return err
		}
		fmt.Println("Revoked. That device is disconnected and can no longer reach this host.")
		return nil
	default:
		return fmt.Errorf("usage: repogo devices [revoke <id>]")
	}
}
