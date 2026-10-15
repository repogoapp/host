// Command tunnelwire compares carriers for the host↔gateway tunnel stream on
// loopback, WebSocket and raw TLS, both carrying tunnelwire's frames.
//
//	go run ./bench/tunnelwire -rtt 0,40ms
package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"runtime/pprof"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/tunnelwire"
)

// The host's streaming chunk and buffered limit, so bulk transfers are framed
// the way forward.go frames them.
const (
	chunkSize     = 512 << 10
	bufferedLimit = 1 << 20
)

// request and response are the tunnel's traffic with the wire format stripped;
// each carrier encodes them into its own messages on every send.
type request struct {
	id    string
	lines []string
}

type response struct {
	id     string
	status int
	lines  []string
	data   []byte
	chunk  bool
	end    bool
}

type carrier interface {
	gatewaySend(request) error
	hostRecv() (request, error)
	hostSend(response) error
	gatewayRecv() (response, error)
	close()
}

func main() {
	rtts := flag.String("rtt", "0,40ms", "comma-separated round-trip delays added by a proxy")
	small := flag.Int("small", 2000, "sequential round trips per run")
	bulk := flag.Int("bulk", 64, "MB streamed per bulk run")
	workers := flag.Int("workers", 64, "requests in flight for the concurrent run")
	window := flag.Duration("for", 3*time.Second, "length of the concurrent run")
	only := flag.String("carrier", "", "run only this carrier")
	profile := flag.String("cpuprofile", "", "write a CPU profile here")
	flag.Parse()
	if *profile != "" {
		f, err := os.Create(*profile)
		if err != nil {
			fail(err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			fail(err)
		}
		defer pprof.StopCPUProfile()
	}

	ctx := context.Background()
	cert, pool := selfSigned()
	carriers := []struct {
		name string
		dial func(context.Context, tls.Certificate, *x509.CertPool, time.Duration) (carrier, error)
	}{
		{"websocket", dialWebSocket},
		{"raw tls", dialTLS},
	}
	for _, field := range strings.Split(*rtts, ",") {
		rtt, err := time.ParseDuration(strings.TrimSpace(field))
		if err != nil {
			fail(err)
		}
		fmt.Printf("\n== added round trip %v\n", rtt)
		fmt.Printf("%-10s %12s %12s %14s %14s %16s\n", "carrier", "2KB p50", "2KB p99", "concurrent", "bulk MB/s", "CPU ms per GB")
		for _, c := range carriers {
			if *only != "" && c.name != *only {
				continue
			}
			conn, err := c.dial(ctx, cert, pool, rtt)
			if err != nil {
				fail(fmt.Errorf("%s: %w", c.name, err))
			}
			b := newBench(conn)
			n := *small
			if rtt > 0 {
				n = max(20, n/100)
			}
			p50, p99 := b.latency(n)
			rps := b.concurrent(*workers, *window)
			mbps, cpu := b.bulk(*bulk)
			fmt.Printf("%-10s %12s %12s %10.0f r/s %14.0f %16.0f\n", c.name, p50.Round(time.Microsecond), p99.Round(time.Microsecond), rps, mbps, cpu)
			conn.close()
		}
	}
}

// ---- the workload ----

type bench struct {
	c       carrier
	mu      sync.Mutex
	waiting map[string]chan response
	next    atomic.Int64
}

// newBench runs the host side: each request is answered on its own goroutine,
// as the host does, with a body of the size its x-size line asks for.
func newBench(c carrier) *bench {
	b := &bench{c: c, waiting: map[string]chan response{}}
	go func() {
		for {
			req, err := c.hostRecv()
			if err != nil {
				return
			}
			go answer(c, req)
		}
	}()
	go func() {
		for {
			resp, err := c.gatewayRecv()
			if err != nil {
				return
			}
			b.mu.Lock()
			ch := b.waiting[resp.id]
			b.mu.Unlock()
			if ch != nil {
				ch <- resp
			}
		}
	}()
	return b
}

var body = func() []byte {
	b := make([]byte, chunkSize)
	_, _ = rand.Read(b)
	return b
}()

var responseLines = []string{
	"Content-Type: text/html; charset=utf-8", "Cache-Control: no-cache, must-revalidate",
	"Vary: rsc, next-router-state-tree, next-router-prefetch", "X-Powered-By: Next.js",
	"Date: Thu, 15 Oct 2026 19:29:35 GMT", "Etag: \"1d2f-18c3a9b7e4\"",
}

func answer(c carrier, req request) {
	size, _ := strconv.Atoi(header(req.lines, "x-size"))
	if size <= bufferedLimit {
		_ = c.hostSend(response{id: req.id, status: 200, lines: responseLines, data: fill(size)})
		return
	}
	if c.hostSend(response{id: req.id, status: 200, lines: responseLines}) != nil {
		return
	}
	for sent := 0; sent < size; sent += chunkSize {
		n := min(chunkSize, size-sent)
		if c.hostSend(response{id: req.id, data: body[:n], chunk: true, end: sent+n >= size}) != nil {
			return
		}
	}
}

func fill(n int) []byte {
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, body[:min(len(body), n-len(out))]...)
	}
	return out
}

func header(lines []string, name string) string {
	for _, line := range lines {
		if k, v, ok := strings.Cut(line, ": "); ok && strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// fetch is one public request through the tunnel; it returns the body bytes
// that came back.
func (b *bench) fetch(size int) int {
	id := "s" + strconv.FormatInt(b.next.Add(1), 10)
	ch := make(chan response, 64)
	b.mu.Lock()
	b.waiting[id] = ch
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.waiting, id)
		b.mu.Unlock()
	}()
	lines := []string{
		"Host: blk7ih3eyg7g.repogo.dev", "User-Agent: Mozilla/5.0 (iPhone; CPU iPhone OS 27_0 like Mac OS X)",
		"Accept: text/html,application/xhtml+xml", "Accept-Language: en-US,en;q=0.9",
		"Cookie: theme=dark; session=4f1c2a9e8b7d6c5e", "Sec-Fetch-Mode: navigate", "x-size: " + strconv.Itoa(size),
	}
	if err := b.c.gatewaySend(request{id: id, lines: lines}); err != nil {
		fail(err)
	}
	got := 0
	for {
		r := <-ch
		got += len(r.data)
		if !r.chunk && size <= bufferedLimit || r.end {
			return got
		}
	}
}

func (b *bench) latency(n int) (p50, p99 time.Duration) {
	b.fetch(2 << 10)
	took := make([]time.Duration, n)
	for i := range took {
		start := time.Now()
		b.fetch(2 << 10)
		took[i] = time.Since(start)
	}
	slices.Sort(took)
	return took[n/2], took[n*99/100]
}

func (b *bench) concurrent(workers int, window time.Duration) float64 {
	var done atomic.Int64
	stop := time.Now().Add(window)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for time.Now().Before(stop) {
				b.fetch(2 << 10)
				done.Add(1)
			}
		})
	}
	wg.Wait()
	return float64(done.Load()) / window.Seconds()
}

// bulk returns the best throughput of three transfers and the process CPU
// time spent per GB, both ends included.
func (b *bench) bulk(mb int) (mbps, cpuPerGB float64) {
	size := mb << 20
	b.fetch(4 << 20)
	var cpus []float64
	for range 3 {
		before := cpuTime()
		start := time.Now()
		if got := b.fetch(size); got != size {
			fail(fmt.Errorf("bulk got %d of %d bytes", got, size))
		}
		secs := time.Since(start).Seconds()
		mbps = max(mbps, float64(mb)/secs)
		cpus = append(cpus, (cpuTime()-before).Seconds()*1000*1024/float64(mb))
	}
	slices.Sort(cpus)
	return mbps, cpus[1]
}

func cpuTime() time.Duration {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

// ---- the tunnel's own frames, as host and gateway encode them ----

func encodeRequest(r request) []byte {
	return encode(&tunnelwire.Request{SessionID: r.id, Method: "GET", Path: "/", Target: "3000", HeaderLines: r.lines})
}

func decodeRequest(b []byte) (request, error) {
	m, err := tunnelwire.Decode(b)
	if err != nil {
		return request{}, err
	}
	r, ok := m.(*tunnelwire.Request)
	if !ok {
		return request{}, fmt.Errorf("got kind %d, not a request", m.Kind())
	}
	return request{id: r.SessionID, lines: r.HeaderLines}, nil
}

func encodeResponse(r response) []byte {
	if r.chunk {
		return encode(&tunnelwire.BodyChunk{SessionID: r.id, EndOfStream: r.end, Data: r.data})
	}
	return encode(&tunnelwire.Response{SessionID: r.id, Status: r.status, HeaderLines: r.lines, Streaming: r.data == nil, Body: r.data})
}

func decodeResponse(b []byte) (response, error) {
	m, err := tunnelwire.Decode(b)
	if err != nil {
		return response{}, err
	}
	switch m := m.(type) {
	case *tunnelwire.BodyChunk:
		return response{id: m.SessionID, data: m.Data, chunk: true, end: m.EndOfStream}, nil
	case *tunnelwire.Response:
		return response{id: m.SessionID, status: m.Status, lines: m.HeaderLines, data: m.Body}, nil
	}
	return response{}, fmt.Errorf("got kind %d, not a response", m.Kind())
}

func encode(m tunnelwire.Message) []byte {
	b, err := tunnelwire.Encode(m)
	if err != nil {
		fail(err)
	}
	return b
}

// ---- WebSocket: coder/websocket on both ends, as the host uses for the relay ----

type wsCarrier struct {
	ctx     context.Context
	server  *http.Server
	host    *websocket.Conn
	gateway *websocket.Conn
	gwMu    sync.Mutex
	hostMu  sync.Mutex
}

func dialWebSocket(ctx context.Context, cert tls.Certificate, pool *x509.CertPool, rtt time.Duration) (carrier, error) {
	lis, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		return nil, err
	}
	accepted := make(chan *websocket.Conn, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		conn.SetReadLimit(tunnelwire.MaxMessage)
		accepted <- conn
		<-r.Context().Done()
	})}
	go server.Serve(lis)
	addr := delayed(lis.Addr().String(), rtt)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost"}}}
	host, _, err := websocket.Dial(ctx, "wss://"+addr+"/_repogo/connect", &websocket.DialOptions{
		HTTPClient: client, CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		return nil, err
	}
	host.SetReadLimit(tunnelwire.MaxMessage)
	return &wsCarrier{ctx: ctx, server: server, host: host, gateway: <-accepted}, nil
}

func (c *wsCarrier) gatewaySend(r request) error {
	c.gwMu.Lock()
	defer c.gwMu.Unlock()
	return c.gateway.Write(c.ctx, websocket.MessageBinary, encodeRequest(r))
}

func (c *wsCarrier) hostRecv() (request, error) {
	_, b, err := c.host.Read(c.ctx)
	if err != nil {
		return request{}, err
	}
	return decodeRequest(b)
}

func (c *wsCarrier) hostSend(r response) error {
	b := encodeResponse(r)
	c.hostMu.Lock()
	defer c.hostMu.Unlock()
	return c.host.Write(c.ctx, websocket.MessageBinary, b)
}

func (c *wsCarrier) gatewayRecv() (response, error) {
	_, b, err := c.gateway.Read(c.ctx)
	if err != nil {
		return response{}, err
	}
	return decodeResponse(b)
}

func (c *wsCarrier) close() {
	_ = c.host.CloseNow()
	_ = c.gateway.CloseNow()
	_ = c.server.Close()
}

// ---- raw TLS: the same frame behind a u32 length ----

type tlsCarrier struct {
	lis     net.Listener
	host    *tlsEnd
	gateway *tlsEnd
}

type tlsEnd struct {
	conn net.Conn
	r    *bufio.Reader
	mu   sync.Mutex
}

// write sends the length and frame as one TLS record.
func (e *tlsEnd) write(frame []byte) error {
	b := make([]byte, 4, 4+len(frame))
	binary.BigEndian.PutUint32(b, uint32(len(frame)))
	b = append(b, frame...)
	e.mu.Lock()
	defer e.mu.Unlock()
	_, err := e.conn.Write(b)
	return err
}

func (e *tlsEnd) read() ([]byte, error) {
	var n [4]byte
	if _, err := io.ReadFull(e.r, n[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size > tunnelwire.MaxMessage {
		return nil, errors.New("frame too large")
	}
	b := make([]byte, size)
	_, err := io.ReadFull(e.r, b)
	return b, err
}

func dialTLS(ctx context.Context, cert tls.Certificate, pool *x509.CertPool, rtt time.Duration) (carrier, error) {
	lis, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		return nil, err
	}
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		// The handshake runs on first use; the dialer waits on it, so run it here.
		if conn.(*tls.Conn).HandshakeContext(ctx) == nil {
			accepted <- conn
		}
	}()
	dialer := tls.Dialer{Config: &tls.Config{RootCAs: pool, ServerName: "localhost"}}
	host, err := dialer.DialContext(ctx, "tcp", delayed(lis.Addr().String(), rtt))
	if err != nil {
		return nil, err
	}
	gateway := <-accepted
	return &tlsCarrier{
		lis:     lis,
		host:    &tlsEnd{conn: host, r: bufio.NewReaderSize(host, 64<<10)},
		gateway: &tlsEnd{conn: gateway, r: bufio.NewReaderSize(gateway, 64<<10)},
	}, nil
}

func (c *tlsCarrier) gatewaySend(r request) error { return c.gateway.write(encodeRequest(r)) }

func (c *tlsCarrier) hostRecv() (request, error) {
	b, err := c.host.read()
	if err != nil {
		return request{}, err
	}
	return decodeRequest(b)
}

func (c *tlsCarrier) hostSend(r response) error { return c.host.write(encodeResponse(r)) }

func (c *tlsCarrier) gatewayRecv() (response, error) {
	b, err := c.gateway.read()
	if err != nil {
		return response{}, err
	}
	return decodeResponse(b)
}

func (c *tlsCarrier) close() {
	_ = c.host.conn.Close()
	_ = c.gateway.conn.Close()
	_ = c.lis.Close()
}

// ---- network delay ----

// delayed returns an address that forwards to target with rtt added, half each
// way. It buffers without limit, so only the carrier's own flow control and
// round trips show, not a TCP window the proxy would add.
func delayed(target string, rtt time.Duration) string {
	if rtt == 0 {
		return target
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fail(err)
	}
	go func() {
		for {
			in, err := lis.Accept()
			if err != nil {
				return
			}
			out, err := net.Dial("tcp", target)
			if err != nil {
				_ = in.Close()
				continue
			}
			go pipe(in, out, rtt/2)
			go pipe(out, in, rtt/2)
		}
	}()
	return lis.Addr().String()
}

func pipe(from, to net.Conn, wait time.Duration) {
	type packet struct {
		data []byte
		at   time.Time
	}
	queue := make(chan packet, 1<<16)
	go func() {
		defer close(queue)
		for {
			buf := make([]byte, 64<<10)
			n, err := from.Read(buf)
			if n > 0 {
				queue <- packet{buf[:n], time.Now().Add(wait)}
			}
			if err != nil {
				return
			}
		}
	}()
	timer := time.NewTimer(0)
	for p := range queue {
		if d := time.Until(p.at); d > 0 {
			timer.Reset(d)
			<-timer.C
		}
		if _, err := to.Write(p.data); err != nil {
			break
		}
	}
	_ = to.Close()
}

// ---- setup ----

func selfSigned() (tls.Certificate, *x509.CertPool) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		fail(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		fail(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		fail(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "tunnelwire:", err)
	os.Exit(1)
}
