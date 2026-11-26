package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/repogoapp/host/internal/hostinfo"
)

// hostCommand is `repogo host rename <name>`: host.rename, the name every
// device shows; "" goes back to the machine's own.
func hostCommand(ctx context.Context, _ *flag.FlagSet, args []string) error {
	// A name is required: leaving it out mustn't clear a saved one.
	if len(args) < 2 || args[0] != "rename" {
		return fmt.Errorf(`usage: repogo host rename <name>   ("" for the machine's own name)`)
	}
	var named hostinfo.Named
	if err := call(ctx, "host.rename", map[string]string{"name": strings.Join(args[1:], " ")}, &named); err != nil {
		return err
	}
	fmt.Printf("This host is now called %s on every device.\n", named.Name)
	return nil
}
