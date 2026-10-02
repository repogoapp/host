package hostinfo

import (
	"context"
	"strings"
	"time"

	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/errkind"
)

// The provisioner names a cloud host's provider and its session's end when it
// starts `repogo serve`; a device that extends the session says so with
// host.set_stops_at, since a running process's environment cannot change.
const (
	CloudKindEnv    = "REPOGO_HOST_KIND"
	CloudStopsAtEnv = "REPOGO_HOST_STOPS_AT"
)

var (
	ErrNotCloud    = errkind.New(errkind.Invalid, "this host has no session to extend")
	ErrStopsAtPast = errkind.New(errkind.Invalid, "stops_at is in the past")
)

// Cloud is a host that runs for a bounded session, such as a Vercel Sandbox:
// who runs it and when it stops, so every device can show and warn about it.
type Cloud struct {
	Provider string    `json:"provider"`
	StopsAt  time.Time `json:"stops_at"`
}

// LoadCloud reads the session from the environment, saving it to path so a
// restart within the session keeps an extension; without the environment it
// reads what path last saved. A Mac sets neither and has no session.
func (s *Service) LoadCloud(path string, getenv func(string) string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cloudPath = path
	provider := strings.TrimSpace(getenv(CloudKindEnv))
	stopsAt := strings.TrimSpace(getenv(CloudStopsAtEnv))
	if provider != "" && stopsAt != "" {
		at, err := time.Parse(time.RFC3339, stopsAt)
		if err != nil {
			return err
		}
		s.cloud = &Cloud{Provider: provider, StopsAt: at.UTC()}
		return s.saveCloud()
	}
	var c Cloud
	found, err := apphome.ReadJSON(path, &c)
	if found && err == nil {
		s.cloud = &c
	}
	return err
}

// Cloud is the session, or nil for a host that runs until it is stopped.
func (s *Service) Cloud() *Cloud {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cloud == nil {
		return nil
	}
	c := *s.cloud
	return &c
}

// SetStopsAt records a session a device extended with its provider.
func (s *Service) SetStopsAt(at time.Time) (Cloud, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cloud == nil {
		return Cloud{}, ErrNotCloud
	}
	if !at.After(time.Now()) {
		return Cloud{}, ErrStopsAtPast
	}
	s.cloud.StopsAt = at.UTC()
	if err := s.saveCloud(); err != nil {
		return Cloud{}, err
	}
	select {
	case s.cloudChanged <- struct{}{}:
	default:
	}
	return *s.cloud, nil
}

// WatchStop calls warn once, `before` the session ends, and again after each
// extension that moves the end, until ctx ends.
func (s *Service) WatchStop(ctx context.Context, before time.Duration, warn func(Cloud)) {
	var warned time.Time
	for {
		c := s.Cloud()
		timer := time.NewTimer(time.Hour)
		if c != nil && !c.StopsAt.Equal(warned) {
			timer.Reset(time.Until(c.StopsAt.Add(-before)))
		} else {
			timer.Stop()
		}
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.cloudChanged:
			timer.Stop()
		case <-timer.C:
			if time.Now().Before(c.StopsAt) {
				warn(*c)
			}
			warned = c.StopsAt
		}
	}
}

// saveCloud writes the session; the caller holds mu.
func (s *Service) saveCloud() error {
	return apphome.WriteJSON(s.cloudPath, s.cloud, 0o600)
}
