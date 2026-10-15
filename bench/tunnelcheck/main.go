// Command tunnelcheck runs the host's tunnel code against a tunnel gateway and
// fetches through it as a visitor would: a page, a 4 MB body and a WebSocket.
//
//	go run ./bench/tunnelcheck                          live: repogo.app opens the link
//	go run ./bench/tunnelcheck -local <gateway binary>  that binary on loopback, Upstash faked
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/tunnel"
)

const bigSize = 4 << 20

func main() {
	local := flag.String("local", "", "path to a tunnel-gateway binary to run on loopback")
	web := flag.String("web", "https://repogo.app", "repogo.app, which opens live links")
	gateway := flag.String("gateway", tunnel.DefaultGateway, "gateway the host dials, live only")
	verbose := flag.Bool("v", false, "show the host's tunnel log")
	flag.Parse()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	host, err := device.Generate()
	if err != nil {
		fail(err)
	}
	phonePub, phoneKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fail(err)
	}
	phone := device.IDFor(phonePub)
	page := "tunnelcheck " + hex.EncodeToString(phonePub[:6])
	port := devServer(page)
	fmt.Printf("host       %s\ndev server 127.0.0.1:%d\n", host.ID, port)

	// Where the host dials, and how a visitor reaches slug.
	var (
		slug, dial string
		expiresAt  int64
		visit      func(slug, path string) (*http.Request, string)
	)
	if *local != "" {
		slug, expiresAt = randomSlug(), time.Now().Add(time.Hour).UnixMilli()
		addr := runLocalGateway(ctx, *local, slug, host.ID, port, expiresAt)
		dial = addr
		visit = func(slug, path string) (*http.Request, string) {
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
			req.Host = slug + "." + tunnel.Domain
			return req, "ws://" + addr + path
		}
	} else {
		dial = *gateway
		var grant struct {
			Slug      string `json:"slug"`
			ExpiresAt int64  `json:"expiresAt"`
		}
		body, _ := json.Marshal(map[string]any{"hostId": string(host.ID), "port": port})
		if err := signed(ctx, *web, phoneKey, http.MethodPost, "/api/v2/tunnels", body, &grant); err != nil {
			fail(fmt.Errorf("repogo.app open: %w", err))
		}
		slug, expiresAt = grant.Slug, grant.ExpiresAt
		defer func() {
			_ = signed(context.Background(), *web, phoneKey, http.MethodDelete, "/api/v2/tunnels/"+slug, nil, nil)
		}()
		visit = func(slug, path string) (*http.Request, string) {
			base := "https://" + slug + "." + tunnel.Domain + path
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base, nil)
			return req, "wss" + strings.TrimPrefix(base, "https")
		}
	}
	fmt.Printf("slug       %s\ngateway    %s\n\n", slug, dial)

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if *verbose {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	dir, err := os.MkdirTemp("", "tunnelcheck")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(dir)
	gatewayHost := dial
	if h, _, err := net.SplitHostPort(dial); err == nil {
		gatewayHost = h
	}
	svc, err := tunnel.Open(tunnel.Config{
		Path: filepath.Join(dir, "tunnels.json"), Identity: host,
		Peers:   peers{phone: {ID: phone, Public: phonePub}},
		Gateway: dial, Plaintext: gatewayHost == "127.0.0.1",
		Changed: func(tunnel.Changed) {}, OwnPort: func() int { return 0 }, Log: logger,
	})
	if err != nil {
		fail(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go svc.Run(runCtx)

	start := time.Now()
	if _, err := svc.Open(phone, slug, port, expiresAt); err != nil {
		fail(err)
	}
	// Run may not be dialing yet, and WaitConnected gives up at once then.
	for !svc.WaitConnected(ctx) {
		if time.Since(start) > 20*time.Second {
			fail(fmt.Errorf("the host never connected to %s", dial))
		}
		time.Sleep(100 * time.Millisecond)
	}
	step("host connected and proved its key", start)

	start = time.Now()
	req, _ := visit(slug, "/")
	got := fetch(req)
	if string(got) != page {
		fail(fmt.Errorf("page: got %q", got))
	}
	step("page through the tunnel", start)

	start = time.Now()
	req, _ = visit(slug, "/big")
	if n := len(fetch(req)); n != bigSize {
		fail(fmt.Errorf("big body: got %d bytes", n))
	}
	took := time.Since(start)
	step(fmt.Sprintf("4 MB body (%.1f MB/s)", float64(bigSize)/(1<<20)/took.Seconds()), start)

	start = time.Now()
	_, wsURL := visit(slug, "/ws")
	opts := &websocket.DialOptions{}
	if *local != "" {
		opts.Host = slug + "." + tunnel.Domain
	}
	conn, _, err := websocket.Dial(ctx, wsURL, opts)
	if err != nil {
		fail(fmt.Errorf("websocket: %w", err))
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte("ping")); err != nil {
		fail(err)
	}
	if _, data, err := conn.Read(ctx); err != nil || string(data) != "echo:ping" {
		fail(fmt.Errorf("websocket echo: %q %v", data, err))
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
	step("websocket round trip", start)

	start = time.Now()
	req, _ = visit("zzzzzzzzzz99", "/")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		fail(fmt.Errorf("a slug nobody opened was served"))
	}
	step(fmt.Sprintf("unknown slug refused (%d)", resp.StatusCode), start)
	fmt.Println("\nOK: the host serves its tunnel through the gateway.")
}

type peers map[device.ID]device.Peer

func (p peers) Peer(id device.ID) (device.Peer, error) {
	peer, ok := p[id]
	if !ok {
		return device.Peer{}, device.ErrUnknownDevice
	}
	return peer, nil
}

func step(name string, start time.Time) {
	fmt.Printf("  ✓ %-40s %v\n", name, time.Since(start).Round(time.Millisecond))
}

func devServer(page string) int {
	big := bytes.Repeat([]byte("0123456789abcdef"), bigSize/16)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, page) })
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(big)
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			typ, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if conn.Write(r.Context(), typ, append([]byte("echo:"), data...)) != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

func randomSlug() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	out := make([]byte, 12)
	for i, c := range b {
		out[i] = "0123456789abcdefghijklmnopqrstuvwxyz"[int(c)%36]
	}
	return string(out)
}

// runLocalGateway starts the gateway binary against a fake Upstash holding
// one row, and returns its address.
func runLocalGateway(ctx context.Context, bin, slug string, hostID device.ID, port int, expiresAt int64) string {
	row, _ := json.Marshal(map[string]any{"hostId": hostID, "target": strconv.Itoa(port), "enabled": true, "expiresAt": expiresAt})
	upstash := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/get/preview:hostname:"+slug {
			_, _ = io.WriteString(w, `{"result":null}`)
			return
		}
		b, _ := json.Marshal(map[string]string{"result": string(row)})
		_, _ = w.Write(b)
	}))
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fail(err)
	}
	addr := lis.Addr().String()
	_, gwPort, _ := net.SplitHostPort(addr)
	_ = lis.Close()
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = append(os.Environ(), "PORT="+gwPort, "TUNNEL_DOMAIN="+tunnel.Domain, "GATEWAY_ID=127.0.0.1",
		"UPSTASH_PREVIEW_REST_URL="+upstash.URL, "UPSTASH_PREVIEW_REST_TOKEN=local")
	if err := cmd.Start(); err != nil {
		fail(err)
	}
	for range 100 {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			return addr
		}
		time.Sleep(50 * time.Millisecond)
	}
	fail(fmt.Errorf("the local gateway never came up"))
	return ""
}

// signed calls repogo.app as a phone would, with a fresh key and nonce.
func signed(ctx context.Context, web string, key ed25519.PrivateKey, method, path string, body []byte, out any) error {
	var nonce struct {
		Nonce string `json:"nonce"`
	}
	if err := call(ctx, http.MethodPost, web+"/api/v2/nonce", nil, nil, &nonce); err != nil {
		return err
	}
	now := strconv.FormatInt(time.Now().UnixMilli(), 10)
	sum := sha256.Sum256(body)
	msg := strings.Join([]string{"repogo-web/1", method, path, hex.EncodeToString(sum[:]), nonce.Nonce, now}, "\n")
	headers := map[string]string{
		"Content-Type":       "application/json",
		"X-Repogo-Key":       base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)),
		"X-Repogo-Signature": base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(msg))),
		"X-Repogo-Nonce":     nonce.Nonce,
		"X-Repogo-Time":      now,
	}
	return call(ctx, method, web+path, body, headers, out)
}

func call(ctx context.Context, method, url string, body []byte, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s %s: %d %s", method, url, resp.StatusCode, b)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

func fetch(req *http.Request) []byte {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		fail(err)
	}
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Errorf("%s: %d %s", req.URL, resp.StatusCode, b))
	}
	return b
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "tunnelcheck:", err)
	os.Exit(1)
}
