package emit_test

import (
	"errors"
	"testing"

	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/emit"
)

type recorder struct{ got []string }

func (r *recorder) Send(to device.ID, method string, _ []byte) error {
	r.got = append(r.got, string(to)+" "+method)
	return nil
}

func TestMuxSendsOnTheAttachedRoute(t *testing.T) {
	direct := &recorder{}
	m := emit.NewMux()
	m.Attach("phone", direct)

	if err := m.Send("phone", "x.y", nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Send("laptop", "x.y", nil); !errors.Is(err, emit.ErrNoRoute) {
		t.Errorf("an unattached device: err = %v, want ErrNoRoute", err)
	}
	if len(direct.got) != 1 || direct.got[0] != "phone x.y" {
		t.Errorf("direct got %v, want the phone's frame", direct.got)
	}
}

func TestMuxDetachOnlyByCurrentHolder(t *testing.T) {
	old, next := &recorder{}, &recorder{}
	m := emit.NewMux()

	m.Attach("phone", old)
	m.Attach("phone", next) // reconnect before the old socket noticed
	m.Detach("phone", old)  // the old socket's deferred cleanup

	if err := m.Send("phone", "x.y", nil); err != nil {
		t.Fatal(err)
	}
	if len(next.got) != 1 {
		t.Errorf("the reconnected route was lost: next=%v", next.got)
	}

	m.Detach("phone", next)
	if err := m.Send("phone", "x.y", nil); !errors.Is(err, emit.ErrNoRoute) {
		t.Errorf("after a real detach: err = %v, want ErrNoRoute", err)
	}
}

func TestMuxWithoutAnyRouteIsAnError(t *testing.T) {
	m := emit.NewMux()
	if err := m.Send("phone", "x.y", nil); !errors.Is(err, emit.ErrNoRoute) {
		t.Errorf("err = %v, want ErrNoRoute", err)
	}
}
