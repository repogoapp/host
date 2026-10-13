package hostlink_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/handshake"
	"github.com/repogoapp/host/internal/jsonrpc"
	"github.com/repogoapp/host/internal/relay"
	"github.com/repogoapp/host/internal/securechan"
	"github.com/repogoapp/host/internal/wsconn"
)

var errPairingRemoved = errors.New("pairing removed")

// A saved pairing must fail explicitly even while a code admits new devices.
func TestSavedPairingDetectsRemovalAndCanPairAgain(t *testing.T) {
	h := newHarness(t)
	phone := generate(t)
	peer := device.Peer{ID: phone.ID, Public: phone.Public, Label: "phone"}
	if err := h.devices.Add(peer); err != nil {
		t.Fatal(err)
	}
	if err := savedPairingCall(t, h, phone); err != nil {
		t.Fatal(err)
	}
	if err := h.devices.Revoke("", phone.ID); err != nil {
		t.Fatal(err)
	}
	for _, pairing := range []bool{false, true} {
		h.Pairing.Store(pairing)
		if err := savedPairingCall(t, h, phone); !errors.Is(err, errPairingRemoved) {
			t.Fatalf("pairing=%v: got %v, want signed removal", pairing, err)
		}
		if n := h.Link.PeerCount(); n != 0 {
			t.Fatalf("refused phone kept %d sessions", n)
		}
	}
	if err := h.devices.Add(peer); err != nil {
		t.Fatal(err)
	}
	if err := savedPairingCall(t, h, phone); err != nil {
		t.Fatalf("paired again: %v", err)
	}
}

func savedPairingCall(t *testing.T, h *harness, phone *device.Identity) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	socket, _, err := websocket.Dial(ctx, h.Relay, nil)
	if err != nil {
		return err
	}
	defer socket.CloseNow()
	ws := wsconn.Wrap(socket)
	_, err = ws.Hello(ctx, handshake.Hello{
		DeviceID: string(phone.ID), PublicKey: phone.Public, GroupID: h.devices.GroupID(),
		Role: handshake.RoleClient, Platform: "ios",
	}, phone.Sign)
	if err != nil {
		return err
	}
	host := h.devices.Identity()
	var request []byte
	send := func(ctx context.Context, payload []byte) error {
		if payload[0] == securechan.Handshake && len(payload) == securechan.Message1Len {
			payload[0] = securechan.PairedHandshake
			request = bytes.Clone(payload)
		}
		wire, err := relay.Encode(host.ID, payload)
		if err != nil {
			return err
		}
		return ws.SendBinary(ctx, wire)
	}
	receive := func(ctx context.Context) ([]byte, error) {
		for {
			_, wire, err := ws.Read(ctx)
			if err != nil {
				return nil, err
			}
			from, payload, err := relay.Decode(wire)
			if err != nil || from != host.ID || len(payload) == 0 {
				continue
			}
			if payload[0] == securechan.NotPaired {
				expected, err := securechan.PairingRejection(host, phone.ID, request)
				if err != nil {
					return nil, err
				}
				if !bytes.Equal(payload, expected) {
					return nil, errors.New("invalid signed refusal")
				}
				return nil, errPairingRemoved
			}
			return payload, nil
		}
	}
	session, err := securechan.Initiate(ctx, phone, host.ID, host.Public, send, receive)
	if err != nil {
		return err
	}
	call, err := jsonrpc.Request("1", "test.ping", nil)
	if err != nil {
		return err
	}
	plainCall, err := jsonrpc.Encode(call)
	if err != nil {
		return err
	}
	if err := session.Seal(plainCall, func(wire []byte) error { return send(ctx, wire) }); err != nil {
		return err
	}
	wire, err := receive(ctx)
	if err != nil {
		return err
	}
	plain, err := session.Open(wire)
	if err != nil {
		return err
	}
	reply, err := jsonrpc.Decode(plain)
	if err != nil {
		return err
	}
	if reply.Error != nil {
		return reply.Error
	}
	return nil
}
