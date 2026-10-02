package main

import (
	"context"
	"fmt"

	"github.com/repogo/host/internal/power"
)

func powerCommand(ctx context.Context, args []string) error {
	sub := "status"
	if len(args) > 0 {
		sub = args[0]
	}
	if len(args) > 1 {
		return fmt.Errorf("unexpected arguments: %v", args[1:])
	}
	switch sub {
	case "status":
		status, err := power.Status(ctx)
		if err != nil {
			return err
		}
		fmt.Println(status)
		return nil
	case "enable":
		if err := power.Enable(ctx); err != nil {
			return err
		}
		fmt.Println("The host now keeps this Mac awake with the lid closed while a device is paired,\n" +
			"unless the Mac runs hot or its battery drops below 20%.")
		return nil
	case "disable":
		if err := power.Disable(ctx); err != nil {
			return err
		}
		fmt.Println("Lid hold removed. The Mac sleeps when its lid closes.")
		return nil
	default:
		return fmt.Errorf("unknown power command %q; use status, enable or disable", sub)
	}
}
