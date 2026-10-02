package emit_test

import (
	"errors"
	"testing"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/emit"
)

type recorder struct{ got []string }

func (r *recorder) Send(to device.ID, method string, _ []byte) error {
	r.got = append(r.got, string(to)+" "+method)
	return nil
}

func TestMuxPrefersAttachedRouteOverFallback(t *testing.T) {
	direct, relay := &recorder{}, &recorder{}
	m := emit.NewMux()
	m.SetFallback(relay)
	m.Attach("phone", direct)

	if err := m.Send("phone", "x.y", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("laptop", "x.y", nil); err != nil {
		t.Fatal(err)
	}
	if len(direct.got) != 1 || direct.got[0] != "phone x.y" {
		t.Errorf("direct got %v, want the phone's frame", direct.got)
	}
	if len(relay.got) != 1 || relay.got[0] != "laptop x.y" {
		t.Errorf("relay got %v, want the unattached device's frame", relay.got)
	}
}

func TestMuxDetachOnlyByCurrentHolder(t *testing.T) {
	old, next, relay := &recorder{}, &recorder{}, &recorder{}
	m := emit.NewMux()
	m.SetFallback(relay)

	m.Attach("phone", old)
	m.Attach("phone", next) // reconnect before the old socket noticed
	m.Detach("phone", old)  // the old socket's deferred cleanup

	if err := m.Send("phone", "x.y", nil); err != nil {
		t.Fatal(err)
	}
	if len(next.got) != 1 {
		t.Errorf("the reconnected route was lost: next=%v relay=%v", next.got, relay.got)
	}

	m.Detach("phone", next)
	if err := m.Send("phone", "x.y", nil); err != nil {
		t.Fatal(err)
	}
	if len(relay.got) != 1 {
		t.Errorf("after a real detach the fallback should carry it; relay=%v", relay.got)
	}
}

func TestMuxWithoutAnyRouteIsAnError(t *testing.T) {
	m := emit.NewMux()
	if err := m.Send("phone", "x.y", nil); !errors.Is(err, emit.ErrNoRoute) {
		t.Errorf("err = %v, want ErrNoRoute", err)
	}
}
