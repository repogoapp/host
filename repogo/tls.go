package main

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/repogoapp/host/internal/device"
)

// tlsCommand is `repogo tls rotate`: a new passthrough TLS key, for one that
// may have leaked. Phones learn the new pin on their next connect.
func tlsCommand(ctx context.Context, args []string) error {
	if len(args) != 1 || args[0] != "rotate" {
		return fmt.Errorf("usage: repogo tls rotate")
	}
	var pin device.SignedPin
	if err := call(ctx, "host.tls_rotate", nil, &pin); err != nil {
		return err
	}
	fmt.Printf("New TLS key %s… (counter %d). Phones on passthrough reconnect, learn it over the relay and refuse the old key from now on.\n",
		hex.EncodeToString(pin.SPKI[:6]), pin.Counter)
	return nil
}
