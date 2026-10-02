// Command relaycheck smoke-tests a LIVE relay: two simulated devices connect,
// prove their keys, and route a message to each other.
//
// It exists because a green health check only proves the process is up. The
// thing that actually breaks in deployment is the WebSocket upgrade surviving a
// proxy and TLS termination, and nothing short of a real round trip tests it.
//
//	go run ./bench/relay -url wss://relay.repogo.app/ws
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/coder/websocket"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/handshake"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/relay"
)

func main() {
	url := flag.String("url", "ws://127.0.0.1:8080/ws", "relay websocket url")
	hold := flag.Int("hold", 0, "seconds to hold the connection open, reading continuously")
	pingBack := flag.Bool("ping", false, "also ping the relay, as the real host does")
	flag.Parse()

	fmt.Printf("relay      %s\n\n", *url)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	host := connect(ctx, *url, handshake.RoleRuntime, "host")
	defer host.ws.CloseNow()
	phone := connect(ctx, *url, handshake.RoleClient, "phone")
	defer phone.ws.CloseNow()

	// Phone → host, addressed by the exact identity the client pinned at pairing.
	send(ctx, phone, host.id, "ping from the phone")
	from, body := recv(ctx, host)
	expect("phone → host", body, "ping from the phone", from, phone.id)

	// Host → phone, addressed by device id.
	send(ctx, host, phone.id, "pong from the host")
	from, body = recv(ctx, phone)
	expect("host → phone", body, "pong from the host", from, host.id)

	fmt.Println("\nOK — the relay routes in both directions over a real connection.")

	if *hold > 0 {
		// Hold with a continuous reader, which is what lets coder/websocket
		// answer the relay's pings — it does not read them itself.
		if *pingBack {
			// Exactly what hostlink does: a keepalive of our own, while the
			// relay is also pinging us.
			go func() {
				t := time.NewTicker(20 * time.Second)
				defer t.Stop()
				for range t.C {
					pctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					err := host.ws.Ping(pctx)
					cancel()
					fmt.Printf("  client ping -> %v\n", err)
					if err != nil {
						return
					}
				}
			}()
		}
		fmt.Printf("\nholding %ds, reading continuously…\n", *hold)
		// NO per-read timeout: coder/websocket CLOSES the connection when a read
		// context expires, so a polling reader kills the very link it is testing.
		readCtx, cancelRead := context.WithTimeout(context.Background(),
			time.Duration(*hold)*time.Second)
		defer cancelRead()

		died := make(chan error, 1)
		go func() {
			for {
				if _, _, err := host.ws.Read(readCtx); err != nil {
					died <- err
					return
				}
			}
		}()

		start := time.Now()
		select {
		case err := <-died:
			if readCtx.Err() == nil {
				fail(fmt.Sprintf("host connection died after %.0fs: %v", time.Since(start).Seconds(), err))
			}
		case <-time.After(time.Duration(*hold) * time.Second):
		}
		fmt.Printf("survived %ds — pings were answered\n", *hold)
	}
}

type peer struct {
	ws   *websocket.Conn
	id   device.ID
	priv ed25519.PrivateKey
}

func connect(ctx context.Context, url string, role, who string) *peer {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	must(err, "generate key")
	id := device.IDFor(pub)

	ws, _, err := websocket.Dial(ctx, url, nil)
	must(err, "dial "+who)

	msg, err := readJSON(ctx, ws)
	must(err, "read challenge")
	if msg.Method != handshake.MethodChallenge {
		fail("expected a challenge, got " + msg.Method)
	}
	var ch handshake.Challenge
	must(jsonrpc.Into(msg.Params, &ch), "decode challenge")

	hello, err := jsonrpc.Request("hello", handshake.MethodHello, handshake.Hello{
		DeviceID:  string(id),
		PublicKey: pub,
		GroupID:   "relaycheck",
		Role:      role,
		Platform:  "relaycheck",
		Label:     who,
		ChallengeSig: ed25519.Sign(priv,
			device.ChallengeMessage(ch.Nonce, ch.ServerID, ch.WallMS)),
	})
	must(err, "build hello")
	must(writeJSON(ctx, ws, hello), "send hello")

	reply, err := readJSON(ctx, ws)
	must(err, "read hello reply")
	if reply.Error != nil {
		fail(fmt.Sprintf("%s rejected: %s", who, reply.Error.Message))
	}
	var accepted handshake.Accepted
	must(jsonrpc.Into(reply.Result, &accepted), "decode hello reply")

	fmt.Printf("%-10s %s  server=%s\n", who, id, accepted.ServerVersion)
	return &peer{ws: ws, id: id, priv: priv}
}

func send(ctx context.Context, p *peer, target device.ID, body string) {
	msg, err := relay.Encode(target, []byte(body))
	must(err, "encode")
	must(p.ws.Write(ctx, websocket.MessageBinary, msg), "send")
}

func recv(ctx context.Context, p *peer) (device.ID, string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, b, err := p.ws.Read(ctx)
	must(err, "receive")
	from, payload, err := relay.Decode(b)
	must(err, "decode")
	return from, string(payload)
}

func expect(step, got, want string, from, wantFrom device.ID) {
	if got != want {
		fail(fmt.Sprintf("%s: body %q, want %q", step, got, want))
	}
	if from != wantFrom {
		fail(fmt.Sprintf("%s: sender %s, want %s", step, from, wantFrom))
	}
	fmt.Printf("%-10s %s  (from %s)\n", "routed", step, from)
}

// The handshake is text; routed traffic is an enveloped binary frame.
func readJSON(ctx context.Context, ws *websocket.Conn) (*jsonrpc.Message, error) {
	_, b, err := ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	return jsonrpc.Decode(b)
}

func writeJSON(ctx context.Context, ws *websocket.Conn, m *jsonrpc.Message) error {
	b, err := jsonrpc.Encode(m)
	if err != nil {
		return err
	}
	return ws.Write(ctx, websocket.MessageText, b)
}

func must(err error, what string) {
	if err != nil {
		fail(what + ": " + err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "FAIL:", msg)
	os.Exit(1)
}
