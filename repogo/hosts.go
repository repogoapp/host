package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/repogoapp/host/internal/remotehost"
)

// hostsCommand lists the other RepoGo hosts the user switched on for this one:
// hosts.list, with `repogo hosts` short for `repogo hosts list`.
func hostsCommand(ctx context.Context, fs *flag.FlagSet, args []string) error {
	if len(args) > 1 || (len(args) == 1 && args[0] != "list") {
		return fmt.Errorf("usage: repogo hosts [list] [--refresh] [--json]")
	}
	params := map[string]any{"refresh": flagBool(fs, "refresh")}
	// A look waits on the SSH login of each host that is switched on, each
	// bounded at a few seconds.
	var raw json.RawMessage
	if err := callWithin(ctx, 20*time.Second, "hosts.list", params, &raw); err != nil {
		return err
	}
	if flagBool(fs, "json") {
		return printJSON(raw)
	}
	var list remotehost.Look
	if err := json.Unmarshal(raw, &list); err != nil {
		return err
	}
	// Listing them, and switching one on, is done in the app by a paired
	// phone: a shell can't say yes for the user, and sees only what is on.
	if len(list.Hosts) == 0 {
		fmt.Println("No other environments are switched on. Switch one on in the app, under Other Environments.")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, h := range list.Hosts {
		detail := h.Reason
		if h.State == remotehost.Ready {
			detail = "repogo " + h.Version
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", h.ID, h.State, detail)
	}
	w.Flush()
	fmt.Printf("Checked %s ago; --refresh looks again.\n", time.Since(time.UnixMilli(list.CheckedAt)).Round(time.Second))
	return nil
}
