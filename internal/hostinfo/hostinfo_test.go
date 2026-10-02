package hostinfo

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/power"
	"github.com/repogo/host/internal/release"
	"github.com/repogo/host/internal/testwait"
)

type reading struct {
	b  power.Battery
	ok bool
}

func (r reading) Battery() (power.Battery, bool) { return r.b, r.ok }

func TestStatusCarriesTheBatteryOnceRead(t *testing.T) {
	status, err := newService(reading{}).Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Battery != nil {
		t.Fatalf("battery %+v before the first reading", status.Battery)
	}
	raw, _ := json.Marshal(status)
	if strings.Contains(string(raw), `"battery"`) {
		t.Fatalf("status %s names a battery it has not read", raw)
	}

	want := power.Battery{Present: true, Percent: 42, Charging: true, PluggedIn: true}
	if status, _ = newService(reading{want, true}).Status(context.Background()); status.Battery == nil || *status.Battery != want {
		t.Fatalf("battery %+v, want %+v", status.Battery, want)
	}
}

func TestStatusCarriesTheNetworkWithoutWaitingOnTheLookup(t *testing.T) {
	lookedUp := make(chan struct{})
	s := New(Deps{Power: reading{}, Relay: func() bool { return false },
		PublicIP: func(context.Context) (string, error) { close(lookedUp); return "203.0.113.7", nil }})
	s.SetPort(7300)

	status, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Network.Port != 7300 || status.Network.PublicIP != "" {
		t.Fatalf("network %+v before the lookup, want port 7300 and no public IP", status.Network)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.WatchPublicIP(ctx)
	<-lookedUp
	testwait.For(t, "the public IP", func() bool {
		status, _ = s.Status(context.Background())
		return status.Network.PublicIP != ""
	})
	if status.Network.PublicIP != "203.0.113.7" {
		t.Fatalf("public IP %q after the lookup", status.Network.PublicIP)
	}
}

func newService(p Battery) *Service {
	return New(Deps{Power: p, Relay: func() bool { return false }})
}

func TestADevelopmentBuildCannotUpdate(t *testing.T) {
	got := newService(reading{}).Release(t.Context())
	if got.Version != "dev" || got.CanUpdate || got.Status != "unknown" {
		t.Fatalf("Release = %+v, want dev, no update, unknown", got)
	}
}

func TestReleaseSaysWhetherTheHostIsBehind(t *testing.T) {
	version, repository := release.Version, release.Repository
	release.Version, release.Repository = "1.2.0", "repogo/host"
	defer func() { release.Version, release.Repository = version, repository }()

	for _, tc := range []struct{ latest, want string }{
		{"", "unknown"}, {"1.2.0", "current"}, {"1.3.0", "behind"},
	} {
		s := newService(reading{})
		// Checked just now, so the test asks GitHub nothing.
		s.checked, s.latest = time.Now(), tc.latest
		if got := s.Release(t.Context()); got.Status != tc.want || !got.CanUpdate {
			t.Errorf("latest %q: %+v, want %s", tc.latest, got, tc.want)
		}
	}
}
