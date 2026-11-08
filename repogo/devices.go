package main

import (
	"context"
	"flag"
	"fmt"
	"time"
)

// devicesCommand lists the paired devices, or revokes one or every one: a
// revoked device's connections close and it can no longer reach this host.
func devicesCommand(ctx context.Context, fs *flag.FlagSet, args []string) error {
	all, yes := flagBool(fs, "all"), flagBool(fs, "yes")
	switch {
	case len(args) == 0 && !all && !yes:
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
	case len(args) == 1 && args[0] == "revoke" && all:
		return revokeAll(ctx, yes)
	case len(args) == 2 && args[0] == "revoke" && !all && !yes:
		if err := call(ctx, "devices.revoke", map[string]string{"id": args[1]}, nil); err != nil {
			return err
		}
		fmt.Println("Revoked. That device is disconnected and can no longer reach this host.")
		return nil
	default:
		return fmt.Errorf("usage: repogo devices [revoke <id> | revoke --all [--yes]]")
	}
}

// revokeAll removes every paired device and ends the reusable invite, so no
// old link pairs one back: for a lost phone, or when something looks wrong.
func revokeAll(ctx context.Context, yes bool) error {
	paired, err := peers(ctx)
	if err != nil {
		return err
	}
	if len(paired) == 0 {
		fmt.Println("No devices paired.")
		return nil
	}
	question := fmt.Sprintf("Remove all %d paired devices, this phone's included? Each one pairs again with repogo pair.", len(paired))
	if err := confirm(question, yes); err != nil {
		return err
	}
	for _, d := range paired {
		if err := call(ctx, "devices.revoke", map[string]string{"id": string(d.ID)}, nil); err != nil {
			return fmt.Errorf("revoking %s (%s): %w", d.Label, d.ID, err)
		}
	}
	if err := call(ctx, "pair.reusable_revoke", nil, nil); err != nil {
		return err
	}
	fmt.Printf("Removed %d devices and ended the reusable invite. Run repogo pair to add yours again.\n", len(paired))
	return nil
}
