// Command cloud measures building the Cloud sheet's cards for a folder.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/repogo/host/internal/clitool"
	"github.com/repogo/host/internal/cloudflare"
	"github.com/repogo/host/internal/cloudprojects"
	"github.com/repogo/host/internal/fly"
	"github.com/repogo/host/internal/vercel"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: go run ./bench/cloud /path/to/apps")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	tools := clitool.NewInventory(nil, nil)
	defer tools.Close()
	tools.Tools = []clitool.Tool{vercel.Tool(), cloudflare.Tool(), fly.Tool()}
	started := time.Now()
	result, err := cloudprojects.New(benchPaths{}, tools.Ready).List(ctx, os.Args[1])
	elapsed := time.Since(started)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	failed := 0
	for _, p := range result.Projects {
		if p.Error != "" {
			failed++
		}
	}
	fmt.Fprintf(os.Stderr, "total %.2f ms\n%d projects, %d unreadable, %d issues\n",
		float64(elapsed)/float64(time.Millisecond), len(result.Projects), failed, len(result.Issues))
	if failed > 0 || len(result.Issues) > 0 {
		os.Exit(2)
	}
}

// The standalone benchmark is explicitly allowed to inspect its supplied root.
type benchPaths struct{}

func (benchPaths) Contain(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}
