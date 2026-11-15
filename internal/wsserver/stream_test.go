package wsserver_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/hostclient"
	"github.com/repogoapp/host/internal/rpc"
	"github.com/repogoapp/host/internal/wsserver"
)

// echoStream sends the request back as two frames, then the device's first
// input as a frame, then ends with an error.
func echoStream(_ context.Context, _ rpc.Caller, request []byte, send func([]byte) error, _ func(any) error, input <-chan []byte) error {
	for range 2 {
		if err := send(request); err != nil {
			return err
		}
	}
	if in, ok := <-input; ok {
		if err := send(in); err != nil {
			return err
		}
	}
	return errors.New("stream over")
}

func streamURL(url string) string { return strings.TrimSuffix(url, "/ws") + "/stream" }

func TestStreamSendsFramesThenWhyItEnded(t *testing.T) {
	r := newRemote(t)
	r.url = streamURL(r.url)
	c, err := r.dial(t, r.pair(t), r.pin, func(cfg *hostclient.Config) { cfg.Stream = []byte(`{"simulator":"Pixel_9"}`) })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := range 2 {
		binary, b, err := c.Next(ctx)
		if err != nil || !binary || string(b) != `{"simulator":"Pixel_9"}` {
			t.Fatalf("frame %d = %v %q %v", i, binary, b, err)
		}
	}
	if err := c.Send(ctx, []byte(`{"touch":"down","x":0.5,"y":0.5}`)); err != nil {
		t.Fatal(err)
	}
	if binary, b, err := c.Next(ctx); err != nil || !binary || string(b) != `{"touch":"down","x":0.5,"y":0.5}` {
		t.Fatalf("input = %v %q %v, want the touch back", binary, b, err)
	}
	binary, b, err := c.Next(ctx)
	var failed wsserver.StreamFailed
	if err != nil || binary || json.Unmarshal(b, &failed) != nil || failed.Error != "stream over" {
		t.Fatalf("end = %v %q %v, want the stream's error as text", binary, b, err)
	}
}

func TestStreamRefusesAnUnpairedDevice(t *testing.T) {
	r := newRemote(t)
	r.url = streamURL(r.url)
	stranger, _ := device.Generate()
	if _, err := r.dial(t, stranger, r.pin, func(cfg *hostclient.Config) { cfg.Stream = []byte(`{}`) }); err == nil {
		t.Fatal("an unpaired device opened a stream")
	}
}

// The relay's listener serves /stream too: a phone with no direct path
// still gets a live view.
func TestRelayListenerServesStream(t *testing.T) {
	r := newRemote(t)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go r.srv.ServeRemote(lis, r.keys.Config())
	t.Cleanup(func() { lis.Close() })
	id := r.pair(t)
	r.url = "wss://" + lis.Addr().String() + "/ws"
	if _, err := r.dial(t, id, r.pin); err != nil {
		t.Fatalf("/ws on the relay's listener: %v", err)
	}
	r.url = streamURL(r.url)
	if _, err := r.dial(t, id, r.pin, func(cfg *hostclient.Config) { cfg.Stream = []byte(`{}`) }); err != nil {
		t.Fatalf("/stream on the relay's listener: %v", err)
	}
}
