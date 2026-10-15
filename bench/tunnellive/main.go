// Command tunnellive measures a LIVE tunnel against the same server reached
// directly, so the difference is what the gateway and the host's stream add.
//
//	go run ./bench/tunnellive -url https://<slug>.repogo.dev -local http://127.0.0.1:3000
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sync"
	"time"
)

var scripts = regexp.MustCompile(`/_next/static/[^"]+\.js`)

func main() {
	public := flag.String("url", "", "public tunnel URL")
	local := flag.String("local", "http://127.0.0.1:3000", "the same server reached directly")
	small := flag.String("small", "/logo.svg", "path of a small static file")
	runs := flag.Int("n", 30, "requests per small measurement")
	flag.Parse()
	if *public == "" {
		fail(fmt.Errorf("-url is required"))
	}
	ctx := context.Background()

	u, err := url.Parse(*public)
	if err != nil {
		fail(err)
	}
	fmt.Printf("tunnel   %s\nlocal    %s\n\n", *public, *local)
	fmt.Printf("network round trip to the gateway (TCP connect)  p50 %v\n\n", connectRTT(ctx, u.Host+":443", 10))

	pageHTML := string(get(ctx, client(), *local+"/"))
	assets := slices.Compact(slices.Sorted(slices.Values(scripts.FindAllString(pageHTML, -1))))
	largest, size := "", 0
	for _, a := range assets {
		if n := len(get(ctx, client(), *local+a)); n > size {
			largest, size = a, n
		}
	}

	fmt.Printf("%-34s %14s %14s %14s\n", "", "local", "tunnel", "added")
	row := func(name string, measure func(base string) time.Duration) {
		l, t := measure(*local), measure(*public)
		fmt.Printf("%-34s %14v %14v %14v\n", name, l.Round(100*time.Microsecond), t.Round(100*time.Microsecond), (t - l).Round(100*time.Microsecond))
	}
	row("small file p50 ("+*small+")", func(base string) time.Duration { return p50(ctx, base+*small, *runs) })
	row("page HTML p50 (/)", func(base string) time.Duration { return p50(ctx, base+"/", 10) })
	row(fmt.Sprintf("largest script p50 (%d KB)", size>>10), func(base string) time.Duration { return p50(ctx, base+largest, 10) })
	row(fmt.Sprintf("all %d scripts at once p50", len(assets)), func(base string) time.Duration { return burst(ctx, base, assets, 5) })

	t := p50(ctx, *public+largest, 10)
	fmt.Printf("\nlargest script through the tunnel  %.1f MB/s\n", float64(size)/(1<<20)/t.Seconds())
}

func client() *http.Client {
	return &http.Client{Timeout: time.Minute, Transport: &http.Transport{
		ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{}, MaxIdleConnsPerHost: 64,
		DisableCompression: true,
	}}
}

func get(ctx context.Context, c *http.Client, target string) []byte {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		fail(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		fail(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		fail(err)
	}
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Errorf("%s: %s", target, resp.Status))
	}
	return b
}

// p50 times requests on one warm connection, so it shows per-request cost and
// not connection setup.
func p50(ctx context.Context, target string, n int) time.Duration {
	c := client()
	get(ctx, c, target)
	took := make([]time.Duration, n)
	for i := range took {
		start := time.Now()
		get(ctx, c, target)
		took[i] = time.Since(start)
	}
	slices.Sort(took)
	return took[n/2]
}

// burst fetches every script in parallel, the way a page load does.
func burst(ctx context.Context, base string, paths []string, n int) time.Duration {
	c := client()
	get(ctx, c, base+paths[0])
	took := make([]time.Duration, n)
	for i := range took {
		start := time.Now()
		var wg sync.WaitGroup
		for _, p := range paths {
			wg.Go(func() { get(ctx, c, base+p) })
		}
		wg.Wait()
		took[i] = time.Since(start)
	}
	slices.Sort(took)
	return took[n/2]
}

func connectRTT(ctx context.Context, addr string, n int) time.Duration {
	var d net.Dialer
	took := make([]time.Duration, n)
	for i := range took {
		start := time.Now()
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			fail(err)
		}
		took[i] = time.Since(start)
		_ = conn.Close()
	}
	slices.Sort(took)
	return took[n/2]
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "tunnellive:", err)
	os.Exit(1)
}
