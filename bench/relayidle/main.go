// Command relayidle measures what one idle socket costs the relay: it opens
// N authenticated sockets that send nothing and reports the relay's memory
// and goroutines per socket.
//
// The relay runs in this process on a Unix socket; the sockets are dialed by
// a child process, so the clients' memory never counts against the relay.
// Loopback TCP can't do this on macOS: it runs out of ports near 16k.
//
//	go run ./bench/relayidle -n 50000
//	go run ./bench/relayidle -n 50000 -heapprofile /tmp/relay.heap
package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/handshake"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/relay"
)

// ipHeader carries a made-up address per socket, so the relay's per-address
// limits see 50k callers rather than one.
const ipHeader = "X-Bench-Client-IP"

func main() {
	n := flag.Int("n", 50000, "idle sockets to open")
	role := flag.String("role", handshake.RoleClient, "role every socket presents: client (phone) or runtime (host)")
	workers := flag.Int("workers", 32, "sockets dialed at once")
	profile := flag.String("heapprofile", "", "write the relay's heap profile here once full")
	dial := flag.String("dial", "", "internal: dial this Unix socket as the child")
	flag.Parse()

	if *dial != "" {
		dialAll(*dial, *n, *role, *workers)
		return
	}
	measure(*n, *role, *workers, *profile)
}

// measure runs the relay, has the child fill it, and reports the difference.
func measure(n int, role string, workers int, profile string) {
	dir, err := os.MkdirTemp("", "relayidle")
	must(err, "temp dir")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "relay.sock")

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv, err := relay.New(relay.Config{ServerID: "relayidle", ClientIPHeader: ipHeader, Log: log})
	must(err, "relay")
	ln, err := net.Listen("unix", sock)
	must(err, "listen")
	go http.Serve(ln, srv.Handler())

	before := sample()
	fmt.Printf("relay      %s\nsockets    %d %s, %d dialing at once\n\n", sock, n, role, workers)

	child := exec.Command(os.Args[0], "-dial", sock, "-n", strconv.Itoa(n), "-role", role, "-workers", strconv.Itoa(workers))
	child.Stderr = os.Stderr
	out, err := child.StdoutPipe()
	must(err, "child stdout")
	must(child.Start(), "start child")
	defer child.Process.Kill()

	start := time.Now()
	ready, err := bufio.NewReader(out).ReadString('\n')
	must(err, "child ended before it was ready")
	if strings.TrimSpace(ready) != "ready" {
		fail("child: " + ready)
	}
	connected := time.Since(start)
	waitFor(sock, n)
	after := sample()

	report(n, connected, before, after)
	if profile != "" {
		f, err := os.Create(profile)
		must(err, "heap profile")
		must(pprof.WriteHeapProfile(f), "heap profile")
		f.Close()
		fmt.Printf("heap profile %s (go tool pprof -top -inuse_space %s)\n", profile, profile)
	}
}

type memory struct {
	heap, stack, sys, rss uint64
	goroutines            int
}

// sample reads the relay process after a collection, so only live memory counts.
func sample() memory {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return memory{heap: m.HeapInuse, stack: m.StackInuse, sys: m.Sys, rss: rss(), goroutines: runtime.NumGoroutine()}
}

// rss is the process's resident memory, from ps, in bytes: the number an
// operator sees, including what the runtime holds but isn't using.
func rss() uint64 {
	b, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0
	}
	kb, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return kb << 10
}

func report(n int, connected time.Duration, before, after memory) {
	per := func(a, b uint64) float64 { return float64(int64(b)-int64(a)) / float64(n) }
	heap, stack := per(before.heap, after.heap), per(before.stack, after.stack)
	live := heap + stack

	fmt.Printf("connected  %d in %s (%.0f/s)\n\n", n, connected.Round(time.Millisecond), float64(n)/connected.Seconds())
	fmt.Printf("%-12s %12s %12s %12s\n", "", "before", "after", "per socket")
	row := func(name string, a, b uint64) {
		fmt.Printf("%-12s %12s %12s %12s\n", name, mib(a), mib(b), kib(per(a, b)))
	}
	row("heap", before.heap, after.heap)
	row("stacks", before.stack, after.stack)
	row("runtime sys", before.sys, after.sys)
	row("rss", before.rss, after.rss)
	fmt.Printf("%-12s %12d %12d %12.2f\n\n", "goroutines", before.goroutines, after.goroutines,
		float64(after.goroutines-before.goroutines)/float64(n))

	fmt.Printf("live per socket  %s (heap + stacks)\n", kib(live))
	fmt.Printf("at 1M sockets    %.1f GiB live, %.1f GiB rss\n", live*1e6/(1<<30), per(before.rss, after.rss)*1e6/(1<<30))
	fmt.Println("\nUser-space only: kernel socket buffers are not counted, and TLS ends at the proxy, not here.")
}

// waitFor polls /stats until the relay has attached every socket: the child
// is ready when its handshakes return, a moment before the last attach.
func waitFor(sock string, n int) {
	client := unixClient(sock)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://relay/stats")
		if err == nil {
			var stats struct{ Connections int }
			err = json.NewDecoder(resp.Body).Decode(&stats)
			resp.Body.Close()
			if err == nil && stats.Connections >= n {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	fail("the relay never attached every socket")
}

// dialAll is the child: open n sockets, say ready, and hold them until killed.
func dialAll(sock string, n int, role string, workers int) {
	client := unixClient(sock)
	var next, done atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := next.Add(1) - 1
				if i >= int64(n) {
					return
				}
				ws, err := connect(client, role, int(i))
				must(err, fmt.Sprintf("socket %d", i))
				// coder/websocket answers pings only while something reads.
				go func() {
					for {
						if _, _, err := ws.Read(context.Background()); err != nil {
							return
						}
					}
				}()
				if d := done.Add(1); d%10000 == 0 {
					fmt.Fprintf(os.Stderr, "  %d open\n", d)
				}
			}
		}()
	}
	wg.Wait()
	fmt.Println("ready")
	select {}
}

// connect opens one socket and completes the relay handshake as a fresh device.
func connect(client *http.Client, role string, i int) (*websocket.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	id := device.IDFor(pub)
	ip := net.IPv4(10, byte(i>>16), byte(i>>8), byte(i)).String()
	ws, _, err := websocket.Dial(ctx, "ws://relay/ws", &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{ipHeader: {ip}},
	})
	if err != nil {
		return nil, err
	}

	challenge, err := readJSON(ctx, ws)
	if err != nil {
		return nil, err
	}
	var ch handshake.Challenge
	if err := jsonrpc.Into(challenge.Params, &ch); err != nil {
		return nil, err
	}
	signed, err := device.ChallengeMessage(ch.Nonce, ch.ServerID, ch.WallMS)
	if err != nil {
		return nil, err
	}
	hello, err := jsonrpc.Request("hello", handshake.MethodHello, handshake.Hello{
		DeviceID:     string(id),
		PublicKey:    pub,
		GroupID:      "relayidle",
		Role:         role,
		Platform:     "relayidle",
		Label:        "idle",
		ChallengeSig: ed25519.Sign(priv, signed),
	})
	if err != nil {
		return nil, err
	}
	b, err := jsonrpc.Encode(hello)
	if err != nil {
		return nil, err
	}
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		return nil, err
	}
	reply, err := readJSON(ctx, ws)
	if err != nil {
		return nil, err
	}
	if reply.Error != nil {
		return nil, fmt.Errorf("rejected: %s", reply.Error.Message)
	}
	return ws, nil
}

func readJSON(ctx context.Context, ws *websocket.Conn) (*jsonrpc.Message, error) {
	_, b, err := ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	return jsonrpc.Decode(b)
}

// unixClient sends every request to the relay's Unix socket, whatever the URL says.
func unixClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
}

func mib(b uint64) string  { return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20)) }
func kib(b float64) string { return fmt.Sprintf("%.1f KiB", b/(1<<10)) }
func must(err error, what string) {
	if err != nil {
		fail(what + ": " + err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "FAIL:", msg)
	os.Exit(1)
}
