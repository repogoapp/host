package main

import (
	"context"
	"encoding/hex"
	"flag"
	"fmt"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/hostinfo"
)

// tlsCommand shows the TLS key phones pin, or rotates it (`repogo tls rotate`)
// for one that may have leaked. No phone trusts a new key, so every one pairs again.
func tlsCommand(ctx context.Context, fs *flag.FlagSet, args []string) error {
	switch {
	case len(args) == 0 && !flagBool(fs, "yes"):
		var status hostinfo.Status
		if err := call(ctx, "host.status", nil, &status); err != nil {
			return err
		}
		if status.TLS == nil {
			fmt.Println("This host has no relay, so no TLS key.")
			return nil
		}
		fmt.Printf("TLS key %s… (counter %d). repogo tls rotate makes a new one.\n", status.TLS.SPKI[:12], status.TLS.Counter)
		return nil
	case len(args) == 1 && args[0] == "rotate":
		if err := confirm("Every paired device will need to pair again. Rotate the TLS key?", flagBool(fs, "yes")); err != nil {
			return err
		}
		var pin device.SignedPin
		if err := call(ctx, "host.tls_rotate", nil, &pin); err != nil {
			return err
		}
		fmt.Printf("New TLS key %s… (counter %d). Pair each device again: repogo pair\n",
			hex.EncodeToString(pin.SPKI[:6]), pin.Counter)
		return nil
	default:
		return fmt.Errorf("usage: repogo tls [rotate [--yes]]")
	}
}
