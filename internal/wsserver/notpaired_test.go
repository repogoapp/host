package wsserver_test

import (
	"errors"
	"testing"
	"time"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/jsonrpc"
)

// notPaired reports whether err is the host's "not paired" refusal.
func notPaired(err error) bool {
	var coded *jsonrpc.Error
	return errors.As(err, &coded) && coded.Code == jsonrpc.CodeNotPaired
}

// A removed phone's next hello says it isn't paired, so it can stop retrying.
func TestARemovedPhoneIsToldItIsNotPaired(t *testing.T) {
	r := newRemote(t)
	phone := r.pair(t)
	if err := r.devices.Revoke("", phone.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.dial(t, phone, r.pin); !notPaired(err) {
		t.Fatalf("dial = %v, want not paired", err)
	}
}

// A key claiming a paired phone's id gets the same answer as an unknown one,
// so a stranger can't learn which ids are paired.
func TestAWrongKeyForAPairedIDLooksUnknown(t *testing.T) {
	r := newRemote(t)
	phone := r.pair(t)
	impostor, _ := device.Generate()
	impostor.ID = phone.ID
	_, claimed := r.dial(t, impostor, r.pin)
	stranger, _ := device.Generate()
	_, unknown := r.dial(t, stranger, r.pin)
	if !notPaired(claimed) || !notPaired(unknown) {
		t.Fatalf("claimed = %v, unknown = %v; want both not paired", claimed, unknown)
	}
	var a, b *jsonrpc.Error
	errors.As(claimed, &a)
	errors.As(unknown, &b)
	if a.Message != b.Message {
		t.Fatalf("messages differ: %q vs %q", a.Message, b.Message)
	}
}

// Removed while a pairing code is live, a phone is let in only to pair; its
// first ordinary call says it isn't paired.
func TestARemovedPhoneDuringALiveCodeIsToldOnItsFirstCall(t *testing.T) {
	r := newRemote(t)
	if _, err := r.pairer.BeginReusable("wss://relay.test/ws", time.Hour); err != nil {
		t.Fatal(err)
	}
	phone, _ := device.Generate()
	c, err := r.dial(t, phone, r.pin)
	if err != nil {
		t.Fatal(err)
	}
	if err := call(t, c, "devices.list", nil, nil); !notPaired(err) {
		t.Fatalf("call = %v, want not paired", err)
	}
}
