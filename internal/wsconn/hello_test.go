package wsconn

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"

	"github.com/repogoapp/host/internal/handshake"
	"github.com/repogoapp/host/internal/jsonrpc"
)

// helloReply runs one handshake whose check answers (unpaired, err) and
// returns the raw reply to the hello.
func helloReply(t *testing.T, unpaired bool, refusal error) *jsonrpc.Message {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c := Wrap(ws)
		if _, err := c.Accept(r.Context(), "test", "dev", func() (string, string) { return "Build Box", "Studio" }, func(handshake.Hello, []byte) (bool, error) {
			return unpaired, refusal
		}); err == nil {
			c.Read(r.Context())
		}
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.CloseNow() })
	c := Wrap(ws)
	if _, err := c.ReadJSON(ctx); err != nil {
		t.Fatal(err)
	}
	hello, err := jsonrpc.Request("hello", handshake.MethodHello, handshake.Hello{DeviceID: "d", Role: handshake.RoleClient})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SendJSON(ctx, hello); err != nil {
		t.Fatal(err)
	}
	reply, err := c.ReadJSON(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return reply
}

// A coded refusal reaches the phone as its code and message only: the cause
// wrapped around it stays on the host.
func TestACodedRefusalSendsOnlyItsCodeAndMessage(t *testing.T) {
	notPaired := &jsonrpc.Error{Code: jsonrpc.CodeNotPaired, Message: "not paired"}
	reply := helloReply(t, false, fmt.Errorf("device auth: device: unknown device: %w", notPaired))
	if reply.Error == nil || reply.Error.Code != jsonrpc.CodeNotPaired || reply.Error.Message != "not paired" {
		t.Fatalf("reply = %+v, want only the coded refusal", reply.Error)
	}
}

func TestAnUncodedRefusalIsDenied(t *testing.T) {
	reply := helloReply(t, false, errors.New("invalid local token"))
	if reply.Error == nil || reply.Error.Code != jsonrpc.CodeDenied {
		t.Fatalf("reply = %+v, want denied", reply.Error)
	}
}

func TestAnUnpairedAdmissionSaysSo(t *testing.T) {
	var accepted handshake.Accepted
	if err := jsonrpc.Into(helloReply(t, true, nil).Result, &accepted); err != nil {
		t.Fatal(err)
	}
	if !accepted.Unpaired {
		t.Fatal("an admission to pair didn't say the device is unpaired")
	}
}

// A paired device hears the host's name as it connects; one let in only to
// pair hasn't proven the code, so it hears neither name.
func TestOnlyAPairedDeviceHearsTheHostsName(t *testing.T) {
	var paired, joining handshake.Accepted
	if err := jsonrpc.Into(helloReply(t, false, nil).Result, &paired); err != nil {
		t.Fatal(err)
	}
	if paired.HostName != "Build Box" || paired.DefaultName != "Studio" {
		t.Fatalf("paired device heard %q / %q", paired.HostName, paired.DefaultName)
	}
	if err := jsonrpc.Into(helloReply(t, true, nil).Result, &joining); err != nil {
		t.Fatal(err)
	}
	if joining.HostName != "" || joining.DefaultName != "" {
		t.Fatalf("a device let in only to pair heard %q / %q", joining.HostName, joining.DefaultName)
	}
}
