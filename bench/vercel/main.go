// Command vercel measures each read behind a Vercel project card, alone and
// as the card would make them together, then two pages of its logs and of
// its deployments.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/repogo/host/internal/vercel"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: go run ./bench/vercel <team id> <project id>")
		os.Exit(1)
	}
	team, project := os.Args[1], os.Args[2]
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for run := 1; run <= 3; run++ {
		started := time.Now()
		p, err := vercel.Read(ctx, team, project)
		fmt.Printf("project  #%d %5.2fs err=%v %+v", run, time.Since(started).Seconds(), err, p)
		if p.Latest != nil {
			fmt.Printf(" latest=%+v", *p.Latest)
		}
		fmt.Println()

		started = time.Now()
		errs, err := vercel.ReadRuntimeErrors(ctx, team, project)
		fmt.Printf("errors   #%d %5.2fs err=%v %+v\n", run, time.Since(started).Seconds(), err, errs)

		started = time.Now()
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = vercel.Read(ctx, team, project) }()
		go func() { defer wg.Done(); _, _ = vercel.ReadRuntimeErrors(ctx, team, project) }()
		wg.Wait()
		fmt.Printf("together #%d %5.2fs\n", run, time.Since(started).Seconds())
	}

	q := vercel.LogQuery{TeamID: team, ProjectID: project, Limit: 10}
	for page := 1; page <= 2; page++ {
		started := time.Now()
		logs, err := vercel.ReadLogs(ctx, q)
		fmt.Printf("logs page %d %5.2fs err=%v entries=%d next=%q", page, time.Since(started).Seconds(), err, len(logs.Entries), logs.NextCursor)
		if n := len(logs.Entries); n > 0 {
			first, last := logs.Entries[0], logs.Entries[n-1]
			fmt.Printf(" newest=%s %s %s %d oldest=%s %s", first.At.Format(time.TimeOnly), first.ID, first.Path, first.Status, last.At.Format(time.TimeOnly), last.ID)
		}
		fmt.Println()
		q.Cursor = logs.NextCursor
	}

	dq := vercel.DeploymentQuery{TeamID: team, ProjectID: project, Limit: 2}
	for page := 1; page <= 2; page++ {
		started := time.Now()
		deploys, err := vercel.ReadDeployments(ctx, dq)
		fmt.Printf("deployments page %d %5.2fs err=%v next=%q\n", page, time.Since(started).Seconds(), err, deploys.NextCursor)
		for _, d := range deploys.Deployments {
			fmt.Printf("  %s %s %s %ds %s %s\n", d.ID, d.Environment, d.State, d.DurationSeconds, d.Commit, d.At.Format(time.DateTime))
		}
		if deploys.NextCursor == "" {
			break
		}
		dq.Cursor = deploys.NextCursor
	}
}
