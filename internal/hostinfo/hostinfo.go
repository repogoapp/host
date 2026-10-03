// Package hostinfo reports host health and available releases.
package hostinfo

import (
	"context"
	"os"
	"sync"
	"time"

	"github.com/repogo/host/internal/emit"
	"github.com/repogo/host/internal/power"
	"github.com/repogo/host/internal/release"
)

func init() {
	emit.Register(power.Battery{})
}

// Status is this running host's health. Whether a newer release is out is
// Release, which host.setup carries.
type Status struct {
	Version        string `json:"version"`
	Executable     string `json:"executable"`
	Uptime         int64  `json:"uptime_seconds"`
	RelayConnected bool   `json:"relay_connected"`
	// UpdateFailed says why the last update was rolled back, for a phone
	// waiting on a restart.
	UpdateFailed string `json:"update_failed,omitempty"`
	// Absent off macOS, and until the host has read the battery once.
	Battery *power.Battery `json:"battery,omitempty"`
	// Cloud is set on a host that runs for a bounded session.
	Cloud   *Cloud  `json:"cloud,omitempty"`
	Network Network `json:"network"`
}

// Battery is the machine's last power reading.
type Battery interface {
	Battery() (power.Battery, bool)
}

type Deps struct {
	Power Battery
	// Relay reports whether the relay link is attached; false on a host without one.
	Relay func() bool
	// UpdateFailed is why the last update was rolled back, reported until the host restarts.
	UpdateFailed string
	// PublicIP looks up the address this machine reaches the internet from: LookupPublicIP.
	PublicIP func(context.Context) (string, error)
}

type Service struct {
	d       Deps
	started time.Time
	mu      sync.Mutex
	latest  string
	checked time.Time
	failed  string

	cloud        *Cloud
	cloudPath    string
	cloudChanged chan struct{}

	port        int
	publicIP    string
	publicAt    time.Time
	publicStale chan struct{}
}

func New(d Deps) *Service {
	return &Service{d: d, started: time.Now(), cloudChanged: make(chan struct{}, 1), publicStale: make(chan struct{}, 1)}
}

// Release is this host's build and whether a newer one is out, in the fields
// an agent's install reports its own.
type Release struct {
	Version string `json:"version"`
	Latest  string `json:"latest,omitempty"`
	// current | behind | unknown: unknown until a check succeeds, and always
	// for a development build.
	Status string `json:"status"`
	// CanUpdate is false for a development build, which host.update refuses.
	CanUpdate   bool   `json:"can_update"`
	UpdateError string `json:"update_error,omitempty"`
	// UpdateFailed says why the last update was rolled back.
	UpdateFailed string `json:"update_failed,omitempty"`
}

// releaseCheckEvery keeps a new release showing soon after it ships, well
// inside GitHub's 60 unauthenticated requests an hour.
const releaseCheckEvery = 10 * time.Minute

// Release checks GitHub at most every releaseCheckEvery and outside the lock;
// a failed check is reported in the release rather than failing it.
func (s *Service) Release(ctx context.Context) Release {
	s.mu.Lock()
	due := time.Since(s.checked) > releaseCheckEvery && release.Repository != ""
	if due {
		s.checked = time.Now()
	}
	s.mu.Unlock()
	if due {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		latest, err := release.Latest(ctx)
		cancel()
		s.mu.Lock()
		if err != nil {
			s.failed = err.Error()
		} else {
			s.latest, s.failed = latest.Version, ""
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Release{
		Version:      release.Version,
		Latest:       s.latest,
		Status:       "unknown",
		CanUpdate:    release.Version != "dev" && release.Repository != "",
		UpdateError:  s.failed,
		UpdateFailed: s.d.UpdateFailed,
	}
	switch {
	case !out.CanUpdate || out.Latest == "":
	case release.Newer(out.Latest, out.Version):
		out.Status = "behind"
	default:
		out.Status = "current"
	}
	return out
}

// Status is the host as it runs now; it asks nothing over the network.
func (s *Service) Status(ctx context.Context) (Status, error) {
	executable, err := os.Executable()
	if err != nil {
		return Status{}, err
	}
	local := localNetwork()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Status{
		Version:        release.Version,
		Executable:     executable,
		Uptime:         int64(time.Since(s.started).Seconds()),
		UpdateFailed:   s.d.UpdateFailed,
		RelayConnected: s.d.Relay(),
		Network:        s.network(local),
	}
	if b, ok := s.d.Power.Battery(); ok {
		out.Battery = &b
	}
	if s.cloud != nil {
		c := *s.cloud
		out.Cloud = &c
	}
	return out, nil
}
