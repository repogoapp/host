package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"time"
)

// callTimeout covers the longest a method waits by design: chats.wait's 5 minutes.
const callTimeout = 6 * time.Minute

// callCommand runs one host method with params read from stdin and prints its
// result as JSON. Generic, so it's how another host runs this one's tools over
// SSH (hosts.call) and how a person calls any method by hand.
func callCommand(ctx context.Context, _ *flag.FlagSet, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: repogo call <method> < params.json")
	}
	params := json.RawMessage("{}")
	if info, err := os.Stdin.Stat(); err == nil && info.Mode()&os.ModeCharDevice == 0 {
		in, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		if len(in) > 0 {
			if !json.Valid(in) {
				return fmt.Errorf("params on stdin aren't JSON")
			}
			params = in
		}
	}
	var out json.RawMessage
	if err := callWithin(ctx, callTimeout, args[0], params, &out); err != nil {
		return err
	}
	return printJSON(out)
}
