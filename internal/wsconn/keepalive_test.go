package wsconn

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// serve accepts one socket, reads it in the background (a pong only arrives
// through the read loop) and runs k on it; dead closes when k declares it.
func serve(t *testing.T, k Keepalive) (url string, dead <-chan struct{}) {
	t.Helper()
	died := make(chan struct{})
	k.Dead = func() { close(died) }
	k.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c := Wrap(ws)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go c.RunKeepalive(ctx, k)
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http"), died
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ws, _, err := websocket.Dial(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	return ws
}

// A peer that never reads never answers a ping. Confirm rechecks a miss at
// once, so it is declared dead before a second Interval could pass; without
// it, not until after one.
func TestConfirmDeclaresASilentPeerDeadQuickly(t *testing.T) {
	const interval = 500 * time.Millisecond
	k := Keepalive{Interval: interval, Timeout: 30 * time.Millisecond, Tolerance: 2}
	for _, confirm := range []time.Duration{10 * time.Millisecond, 0} {
		t.Run(fmt.Sprint("confirm=", confirm), func(t *testing.T) {
			t.Parallel()
			k := k
			k.Confirm, k.ConfirmTimeout = confirm, k.Timeout
			url, dead := serve(t, k)
			start := time.Now()
			dial(t, url)

			select {
			case <-dead:
				if confirm == 0 {
					t.Fatalf("declared dead after %v without Confirm, before a second interval", time.Since(start))
				}
				return
			case <-time.After(2 * interval):
				if confirm > 0 {
					t.Fatal("a silent peer was not declared dead before a second interval")
				}
			}
			select {
			case <-dead:
			case <-time.After(10 * interval):
				t.Fatal("a silent peer was never declared dead")
			}
		})
	}
}

// A peer that keeps sending is not pinged, so one that cannot answer pings
// (it never reads) is still alive while its frames arrive.
func TestABusyPeerIsNotPinged(t *testing.T) {
	url, dead := serve(t, Keepalive{
		Interval: 100 * time.Millisecond, Timeout: 50 * time.Millisecond, Tolerance: 2,
		Confirm: 20 * time.Millisecond, ConfirmTimeout: 50 * time.Millisecond, Idle: true,
	})
	ws := dial(t, url)

	tick := time.NewTicker(30 * time.Millisecond)
	defer tick.Stop()
	stop := time.After(500 * time.Millisecond)
	for {
		select {
		case <-dead:
			t.Fatal("a busy peer was pinged and declared dead")
		case <-stop:
			return
		case <-tick.C:
			if err := ws.Write(context.Background(), websocket.MessageBinary, []byte("x")); err != nil {
				t.Fatal(err)
			}
		}
	}
}
