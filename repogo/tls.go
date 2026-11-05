package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/repogoapp/host/internal/device"
)

// tlsCommand is `repogo tls rotate`: a new TLS key for phones to pin, for one
// that may have leaked. No phone trusts the new key, so every one pairs again.
func tlsCommand(ctx context.Context, args []string) error {
	if len(args) != 1 || args[0] != "rotate" {
		return fmt.Errorf("usage: repogo tls rotate")
	}
	fmt.Print("Every paired device will need to pair again. Rotate the TLS key? [y/N] ")
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
		fmt.Println("Left as it was.")
		return nil
	}
	var pin device.SignedPin
	if err := call(ctx, "host.tls_rotate", nil, &pin); err != nil {
		return err
	}
	fmt.Printf("New TLS key %s… (counter %d). Pair each device again: repogo pair\n",
		hex.EncodeToString(pin.SPKI[:6]), pin.Counter)
	return nil
}
