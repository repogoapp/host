// Package tunnel serves public `<slug>.<Domain>` URLs for this host's dev
// servers, and `h-<host id>.<Domain>` for its plugins, through RepoGo's
// tunnel gateway. repogo.app decides who
// may open a tunnel and writes the gateway's row; this host decides what it
// serves: only a slug a paired device opened here, only to that slug's port,
// only on 127.0.0.1. It dials the gateway only while a tunnel is open, and
// proves its device key there so no one else can take its traffic.
package tunnel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/apphome"
	"github.com/repogoapp/host/internal/device"
	"github.com/repogoapp/host/internal/errkind"
)

var (
	ErrInvalid  = errkind.New(errkind.Invalid, "invalid tunnel")
	ErrNotFound = errkind.New(errkind.NotFound, "no such tunnel")
)

// Domain is where the gateway serves tunnels.
const Domain = "tunnels.repogo.dev"

// DefaultGateway is where the host opens its gateway socket; its host name is
// signed into every proof, so a proof made for it is useless anywhere else.
const DefaultGateway = "tunnels.repogo.dev"

// MaxDuration matches repogo.app's longest tunnel, with room for clock skew.
const MaxDuration = 7*24*time.Hour + 5*time.Minute

// Slugs are repogo.app's random base36 names.
var slugPattern = regexp.MustCompile(`^[0-9a-z]{10,32}$`)

// Tunnel is one public URL this host serves.
type Tunnel struct {
	Slug      string `json:"slug"`
	URL       string `json:"url"`
	Port      int    `json:"port"`
	ExpiresAt int64  `json:"expires_at"` // Unix ms, as repogo.app granted it
	OpenedAt  int64  `json:"opened_at"`
}

// entry is a tunnel as saved: with the device that opened it, which only
// this host needs, to end the tunnel when that device is unpaired.
type entry struct {
	Tunnel
	OpenedBy device.ID `json:"opened_by"`
}

// Peers is the paired devices; a tunnel lasts only while its opener is paired.
type Peers interface {
	Peer(device.ID) (device.Peer, error)
}

type Config struct {
	// Path is the allowlist file.
	Path     string
	Identity *device.Identity
	Peers    Peers
	// Gateway is a host name with an optional port; Plaintext skips TLS, for a
	// local gateway or a test.
	Gateway   string
	Plaintext bool
	// Changed is told the whole list after every change, for tunnels.changed.
	Changed func(Changed)
	// OwnPort is the host's own loopback port, read at each open since the
	// listener binds after this service is built. A tunnel to it is refused.
	OwnPort func() int
	// Plugins serves the environment's own address; nil serves nothing there.
	Plugins Plugins
	Log     *slog.Logger
}

type file struct {
	Tunnels []entry `json:"tunnels"`
}

type Service struct {
	cfg  Config
	now  func() time.Time
	wake chan struct{}

	mu        sync.Mutex
	tunnels   map[string]entry
	connected bool
	running   bool          // Run is dialing the gateway
	attempt   chan struct{} // closed and replaced as each gateway attempt connects or ends
	link      *link         // the live gateway stream, if any
}

func Open(cfg Config) (*Service, error) {
	s := &Service{cfg: cfg, now: time.Now, wake: make(chan struct{}, 1), attempt: make(chan struct{}), tunnels: map[string]entry{}}
	var f file
	if _, err := apphome.ReadJSON(cfg.Path, &f); err != nil {
		return nil, err
	}
	for _, e := range f.Tunnels {
		s.tunnels[e.Slug] = e
	}
	return s, nil
}

// Open allows slug to reach port until expiresAt. The phone calls it after
// repogo.app granted the slug; opening the same slug again updates it.
func (s *Service) Open(by device.ID, slug string, port int, expiresAt int64) (Tunnel, error) {
	now := s.now()
	switch {
	case !slugPattern.MatchString(slug):
		return Tunnel{}, fmt.Errorf("%w: slug must be 10 to 32 lowercase letters and digits", ErrInvalid)
	case port < 1024 || port > 65535:
		return Tunnel{}, fmt.Errorf("%w: port must be from 1024 to 65535", ErrInvalid)
	case port == s.cfg.OwnPort():
		return Tunnel{}, fmt.Errorf("%w: port %d is the host's own", ErrInvalid, port)
	case expiresAt <= now.UnixMilli() || expiresAt > now.Add(MaxDuration).UnixMilli():
		return Tunnel{}, fmt.Errorf("%w: expires_at must be within seven days from now", ErrInvalid)
	}
	t := Tunnel{Slug: slug, URL: "https://" + slug + "." + Domain, Port: port, ExpiresAt: expiresAt, OpenedAt: now.UnixMilli()}
	s.mu.Lock()
	s.tunnels[slug] = entry{Tunnel: t, OpenedBy: by}
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return Tunnel{}, err
	}
	s.changed()
	return t, nil
}

// Close stops serving slug at once; repogo.app's row may outlive it, and is
// then refused here.
func (s *Service) Close(slug string) error {
	s.mu.Lock()
	if _, ok := s.tunnels[slug]; !ok {
		s.mu.Unlock()
		return ErrNotFound
	}
	delete(s.tunnels, slug)
	err := s.saveLocked()
	l := s.link
	s.mu.Unlock()
	if l != nil {
		l.closeSlug(slug)
	}
	if err != nil {
		return err
	}
	s.changed()
	return nil
}

// ClosePort closes every tunnel serving port.
func (s *Service) ClosePort(port int) error {
	for _, t := range s.List() {
		if t.Port != port {
			continue
		}
		if err := s.Close(t.Slug); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	return nil
}

// List is the open tunnels, soonest to expire first.
func (s *Service) List() []Tunnel {
	s.prune()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked()
}

// URLs is the public URL serving each local port; the one expiring last wins
// when a port has two.
func (s *Service) URLs() map[int]string {
	out := map[int]string{}
	for _, t := range s.List() {
		out[t.Port] = t.URL
	}
	return out
}

// Status is the list and whether the gateway is reachable right now.
func (s *Service) Status() Changed {
	s.prune()
	s.mu.Lock()
	defer s.mu.Unlock()
	return Changed{Tunnels: s.listLocked(), Connected: s.connected}
}

func (s *Service) listLocked() []Tunnel {
	out := make([]Tunnel, 0, len(s.tunnels))
	for _, e := range s.tunnels {
		out = append(out, e.Tunnel)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ExpiresAt != out[j].ExpiresAt {
			return out[i].ExpiresAt < out[j].ExpiresAt
		}
		return out[i].Slug < out[j].Slug
	})
	return out
}

// allowed is the port slug may reach, if it is open, unexpired, and its
// opener is still paired.
func (s *Service) allowed(slug string) (int, bool) {
	s.mu.Lock()
	e, ok := s.tunnels[slug]
	s.mu.Unlock()
	if !ok || !s.live(e) {
		return 0, false
	}
	return e.Port, true
}

func (s *Service) live(e entry) bool {
	if e.ExpiresAt <= s.now().UnixMilli() {
		return false
	}
	_, err := s.cfg.Peers.Peer(e.OpenedBy)
	return err == nil
}

// prune drops expired tunnels and those whose opener was unpaired.
func (s *Service) prune() {
	s.mu.Lock()
	var dropped bool
	for slug, e := range s.tunnels {
		if !s.live(e) {
			delete(s.tunnels, slug)
			dropped = true
		}
	}
	var err error
	if dropped {
		err = s.saveLocked()
	}
	s.mu.Unlock()
	if err != nil {
		s.cfg.Log.Warn("tunnel: saving the pruned list failed", "err", err)
	}
	if dropped {
		s.changed()
	}
}

// connectWait bounds WaitConnected for a gateway that neither answers nor refuses.
const connectWait = 10 * time.Second

// WaitConnected waits for the gateway stream, so a URL handed out right after
// Open is already served. It gives up when an attempt fails, at connectWait,
// or at once when Run isn't dialing.
func (s *Service) WaitConnected(ctx context.Context) bool {
	s.mu.Lock()
	connected, running, attempt := s.connected, s.running, s.attempt
	s.mu.Unlock()
	if connected || !running {
		return connected
	}
	ctx, cancel := context.WithTimeout(ctx, connectWait)
	defer cancel()
	select {
	case <-attempt:
	case <-ctx.Done():
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connected
}

func (s *Service) setConnected(on bool) {
	s.mu.Lock()
	moved := s.connected != on
	s.connected = on
	close(s.attempt)
	s.attempt = make(chan struct{})
	s.mu.Unlock()
	if moved {
		s.changed()
	}
}

func (s *Service) changed() {
	s.Wake()
	s.mu.Lock()
	ev := Changed{Tunnels: s.listLocked(), Connected: s.connected}
	s.mu.Unlock()
	s.cfg.Changed(ev)
}

func (s *Service) saveLocked() error {
	saved := make([]entry, 0, len(s.tunnels))
	for _, t := range s.listLocked() {
		saved = append(saved, s.tunnels[t.Slug])
	}
	return apphome.WriteJSON(s.cfg.Path, file{Tunnels: saved}, 0o600)
}

// pruneEvery is how often an idle or connected host re-checks expiry.
const pruneEvery = time.Minute

// Run holds the gateway connection open while any tunnel is or a plugin
// serves HTTP, and drops it when neither is left.
func (s *Service) Run(ctx context.Context) {
	s.setRunning(true)
	defer s.setRunning(false)
	for ctx.Err() == nil {
		if !s.wanted() {
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
			case <-time.After(pruneEvery):
			}
			continue
		}
		linkCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			s.dialLoop(linkCtx)
		}()
		for s.wanted() && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-s.wake:
			case <-time.After(pruneEvery):
			}
		}
		cancel()
		<-done
		s.setConnected(false)
	}
}

// wanted reports whether anything needs the gateway: an open tunnel, or a
// plugin route at the environment's address.
func (s *Service) wanted() bool {
	return len(s.List()) > 0 || (s.cfg.Plugins != nil && s.cfg.Plugins.HasRoutes())
}

// Wake re-checks at once whether the gateway is needed, for a plugin that
// just gained or lost its HTTP routes.
func (s *Service) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *Service) setRunning(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = on
}
