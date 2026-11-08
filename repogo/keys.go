package main

import (
	"cmp"
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/repogoapp/host/internal/plugins"
)

// keysCommand shows each environment plugin's key for its HTTP methods that
// take one, or one plugin's, making it if it has none: showing never replaces
// a key, so pasting the command is always safe. --new replaces, --delete removes.
func keysCommand(ctx context.Context, fs *flag.FlagSet, args []string) error {
	replace, remove := flagBool(fs, "new"), flagBool(fs, "delete")
	switch {
	case len(args) == 0 && !replace && !remove:
		keyed, err := keyedPlugins(ctx)
		if err != nil {
			return err
		}
		if len(keyed) == 0 {
			fmt.Println(`No plugin has a method that takes a key. Give one http: { public: "key" } or { device: "key" }.`)
			return nil
		}
		for _, p := range keyed {
			key := cmp.Or(p.Key, "no key yet: repogo keys "+p.Plugin)
			fmt.Printf("  %s  %s\n    methods: %s\n", p.Plugin, key, strings.Join(p.Methods, ", "))
		}
		return nil
	case len(args) == 1 && remove && !replace:
		if err := call(ctx, "plugins.delete_key", map[string]string{"plugin": args[0]}, nil); err != nil {
			return err
		}
		fmt.Println("Deleted. Its methods that take a key answer no one until you run repogo keys " + args[0] + " again.")
		return nil
	case len(args) == 1 && !remove:
		if !replace {
			keyed, err := keyedPlugins(ctx)
			if err != nil {
				return err
			}
			for _, p := range keyed {
				if p.Plugin == args[0] && p.Key != "" {
					fmt.Println(p.Key)
					return nil
				}
			}
		}
		var made plugins.KeyedPlugin
		if err := call(ctx, "plugins.create_key", map[string]string{"plugin": args[0]}, &made); err != nil {
			return err
		}
		fmt.Println(made.Key)
		if replace {
			fmt.Println("\nThe old key stops working now.")
		}
		return nil
	default:
		return fmt.Errorf("usage: repogo keys [<plugin> [--new | --delete]]")
	}
}

func keyedPlugins(ctx context.Context) ([]plugins.KeyedPlugin, error) {
	var list struct {
		Plugins []plugins.KeyedPlugin `json:"plugins"`
	}
	err := call(ctx, "plugins.keys", nil, &list)
	return list.Plugins, err
}
