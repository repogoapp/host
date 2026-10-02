// Command tools measures the cloud CLI checks behind host.setup: each CLI
// alone, then all together as the setup snapshot runs them.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/repogo/host/internal/clitool"
	"github.com/repogo/host/internal/cloudflare"
	"github.com/repogo/host/internal/fly"
	"github.com/repogo/host/internal/vercel"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	in := clitool.NewInventory(nil, nil)
	defer in.Close()
	in.Tools = []clitool.Tool{vercel.Tool(), cloudflare.Tool(), fly.Tool()}

	for _, t := range in.Tools {
		started := time.Now()
		got, _ := in.Get(ctx, t.Kind)
		fmt.Printf("%-11s %6.2fs installed=%v version=%s authed=%v status=%s\n",
			t.Kind, time.Since(started).Seconds(), got.Installed, got.Version, got.Authed, got.Status)
	}
	for run := 1; run <= 3; run++ {
		started := time.Now()
		in.List(ctx)
		fmt.Printf("list #%d    %6.2fs\n", run, time.Since(started).Seconds())
	}
}
