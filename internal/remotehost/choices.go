package remotehost

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/apphome"
	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/errkind"
)

func init() { emit.Register(Changed{}) }

// AuthorizedFile names, in the host's state folder, what the user chose:
// whether their SSH connections are listed, and the ones switched on.
const AuthorizedFile = "authorized-hosts.json"

// Changed is hosts.changed: the list was turned on or off, or a host was
// switched, so a device showing hosts.list reads it again.
type Changed struct{}

func (Changed) Method() string { return "hosts.changed" }

// Authorized is one host the user switched on, by its ~/.ssh/config alias:
// the user's yes to this host connecting there. HostID is what it answered
// the first time it did, empty until then; a machine that later answers at
// that alias with another id is off again.
type Authorized struct {
	Alias  string `json:"alias"`
	HostID string `json:"host_id,omitempty"`
	// AddedAt is when the user switched it on, in ms.
	AddedAt int64 `json:"added_at"`
}

// saved is the file.
type saved struct {
	// Listing is the user's yes to reading ~/.ssh/config for the list.
	Listing bool         `json:"listing"`
	Hosts   []Authorized `json:"hosts"`
}

// choices is the saved file, read once.
type choices struct {
	path string

	mu     sync.Mutex
	loaded bool
	saved  saved
}

// load reads the file once. One that is missing or can't be read is nothing
// chosen: no file is read and nothing is dialed that the user didn't pick.
func (c *choices) load() {
	if c.loaded {
		return
	}
	c.loaded = true
	if c.path == "" {
		return
	}
	var file saved
	if _, err := apphome.ReadJSON(c.path, &file); err == nil {
		c.saved = file
	}
}

// listing reports whether the user asked for the list.
func (c *choices) listing() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	return c.saved.Listing
}

// setListing turns the list on or off, reporting whether that changed it.
// Off switches every host off with it, and answers their aliases.
func (c *choices) setListing(on bool) (changed bool, dropped []string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	if c.saved.Listing == on {
		return false, nil, nil
	}
	next := saved{Listing: on, Hosts: c.saved.Hosts}
	if !on {
		for _, h := range next.Hosts {
			dropped = append(dropped, h.Alias)
		}
		next.Hosts = nil
	}
	return true, dropped, c.save(next)
}

// has reports whether the user switched alias on.
func (c *choices) has(alias string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	return c.index(alias) >= 0
}

// alias is the switched-on alias a caller named, ignoring case; false for
// any other name, without looking at what is listed.
func (c *choices) alias(named string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	i := slices.IndexFunc(c.saved.Hosts, func(e Authorized) bool { return strings.EqualFold(e.Alias, named) })
	if i < 0 {
		return "", false
	}
	return c.saved.Hosts[i].Alias, true
}

func (c *choices) index(alias string) int {
	if alias == "" {
		return -1
	}
	return slices.IndexFunc(c.saved.Hosts, func(e Authorized) bool { return e.Alias == alias })
}

// set switches alias on or off and saves, reporting whether anything changed.
// Nothing is switched on while the list is off, whichever was asked first.
func (c *choices) set(alias string, on bool, now time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	if on && !c.saved.Listing {
		return false, errkind.New(errkind.Denied, "connections aren't allowed on this environment")
	}
	i := c.index(alias)
	if on == (i >= 0) {
		return false, nil
	}
	next := saved{Listing: c.saved.Listing, Hosts: slices.Clone(c.saved.Hosts)}
	if on {
		next.Hosts = append(next.Hosts, Authorized{Alias: alias, AddedAt: now.UnixMilli()})
	} else {
		next.Hosts = slices.Delete(next.Hosts, i, i+1)
	}
	return true, c.save(next)
}

// pin keeps the id alias answered with the first time it does, and reports
// whether hostID is the one kept. Another id is another machine at that
// alias: it is switched off, for the user to switch on again if they meant it.
func (c *choices) pin(alias, hostID string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	i := c.index(alias)
	if i < 0 || c.saved.Hosts[i].HostID == hostID {
		return i >= 0, nil
	}
	next := saved{Listing: c.saved.Listing, Hosts: slices.Clone(c.saved.Hosts)}
	if next.Hosts[i].HostID != "" {
		next.Hosts = slices.Delete(next.Hosts, i, i+1)
		return false, c.save(next)
	}
	next.Hosts[i].HostID = hostID
	return true, c.save(next)
}

func (c *choices) save(next saved) error {
	if c.path != "" {
		if err := apphome.WriteJSON(c.path, next, 0o600); err != nil {
			return fmt.Errorf("save the other environments: %w", err)
		}
	}
	c.saved = next
	return nil
}

// SetListing turns the list of the user's SSH connections on or off, and
// answers it as it is then. On is their yes to reading ~/.ssh/config; off
// switches every host off and reads it no more.
func (s *Service) SetListing(ctx context.Context, on bool) (Look, error) {
	changed, dropped, err := s.choices.setListing(on)
	if err != nil {
		return Look{}, err
	}
	s.d.debug("hosts: listing", "on", on, "changed", changed, "switched_off", len(dropped))
	if changed {
		for _, alias := range dropped {
			s.hangUp(ctx, alias)
		}
		s.look(ctx, true)
		s.d.Changed()
	}
	return s.List(ctx, false), nil
}

// Authorize switches the host hosts.list calls id on or off, and answers it
// as it is then. On is the user's yes to dialing it, so any listed host can
// be switched on, one that is offline or has no RepoGo yet included: it is
// checked now, and again on each look.
func (s *Service) Authorize(ctx context.Context, id string, on bool) (Host, error) {
	h, ok := s.find(ctx, id)
	if !ok {
		return Host{}, errkind.New(errkind.NotFound, "no SSH connection called "+id+" is listed")
	}
	changed, err := s.choices.set(h.ID, on, s.now())
	if err != nil {
		return Host{}, err
	}
	s.d.debug("hosts: switched", "alias", h.ID, "on", on, "changed", changed)
	if changed {
		if !on {
			s.hangUp(ctx, h.ID)
		}
		s.look(ctx, true)
		s.d.Changed()
	}
	if now, ok := s.find(ctx, h.ID); ok {
		return now, nil
	}
	h.Authorized = on
	return h, nil
}

// find is the host hosts.list calls id, ignoring case, usable or not, from
// the last look at any age; an id that look doesn't have runs one look.
func (s *Service) find(ctx context.Context, id string) (Host, bool) {
	s.mu.Lock()
	last := s.last
	s.mu.Unlock()
	h, ok := named(last.Hosts, id)
	if !ok {
		h, ok = named(s.look(ctx, true).Hosts, id)
	}
	return s.marked(h), ok
}

// marked is h with whether the user switched it on.
func (s *Service) marked(h Host) Host {
	h.Authorized = s.choices.has(h.ID)
	return h
}
