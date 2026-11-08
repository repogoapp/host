package main

import (
	"context"
	"flag"
	"fmt"

	"github.com/repogoapp/host/internal/plugins"
)

// urlsCommand lists the plugin methods that answer plain HTTP and their URLs,
// every plugin's or one's; the key a URL needs is repogo keys <plugin>.
func urlsCommand(ctx context.Context, _ *flag.FlagSet, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: repogo urls [<plugin>]")
	}
	params := map[string]string{}
	if len(args) == 1 {
		params["plugin"] = args[0]
	}
	var list struct {
		Methods []plugins.ServedMethod `json:"methods"`
	}
	if err := call(ctx, "plugins.urls", params, &list); err != nil {
		return err
	}
	if len(list.Methods) == 0 {
		fmt.Println(`No plugin method answers HTTP. Give one http: { device: "none" } or { public: "key" }.`)
		return nil
	}
	plugin, needsKey := "", false
	for _, m := range list.Methods {
		if m.Plugin != plugin {
			plugin = m.Plugin
			fmt.Println(plugin)
		}
		for i, u := range m.URLs {
			name, key := m.Method, ""
			if i > 0 {
				name = ""
			}
			if u.Key {
				key, needsKey = "  key", true
			}
			fmt.Printf("  %-14s %-7s %s%s\n", name, u.Address, u.URL, key)
		}
	}
	if needsKey {
		fmt.Println("\nSend a key URL's key as Authorization: Bearer <key>; repogo keys <plugin> prints it.")
	}
	return nil
}
