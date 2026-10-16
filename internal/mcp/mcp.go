// Package mcp is the MCP servers a user connects through RepoGo: added by URL
// from the phone, stored with their credentials on this host only, and handed
// to Claude and Codex turns in the projects they are switched on for.
//
// Nothing here leaves the machine except over the paired device's sealed
// channel, and credentials never do: Server is the public view, record holds
// the secret.
package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/apphome"
	"github.com/repogoapp/host/internal/errkind"
)

var (
	// ErrInvalid is a request the caller can fix, or an upstream's refusal
	// quoted to them: a bad URL, a missing key, a sign-in that failed.
	ErrInvalid  = errkind.New(errkind.Invalid, "invalid mcp request")
	ErrNotFound = errkind.New(errkind.NotFound, "no such mcp server")
)

// AuthKind is how a server authenticates, as the probe found it; the only
// branch in the feature.
type AuthKind string

const (
	AuthNone   AuthKind = "none"
	AuthAPIKey AuthKind = "api_key"
	AuthOAuth  AuthKind = "oauth"
)

// Server is what a device sees: never a credential.
type Server struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	URL   string   `json:"url"`
	Auth  AuthKind `json:"auth"`
	// Project roots this server is switched on for. Opt-in: a server costs
	// context in every turn it rides, so it is on only where the user said.
	Projects  []string `json:"projects" wire:"array"`
	CreatedAt int64    `json:"created_at"`
}

// record is a Server as stored, with what it takes to call it.
type record struct {
	Server
	// Set when an API key goes in a header other than Authorization.
	HeaderName string       `json:"header_name,omitempty"`
	OAuth      *oauthClient `json:"oauth,omitempty"`
	Secret     string       `json:"secret,omitempty"`
	Refresh    string       `json:"refresh,omitempty"`
	ExpiresAt  int64        `json:"expires_at,omitempty"` // unix ms; 0 is long-lived
}

// Builtin is a server this host serves itself: on by default, switched for
// the whole environment, never renamed or removed.
type Builtin struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	On          bool   `json:"on"`
}

// BuiltinID is RepoGo's own server, internal/repogomcp; also its name in a
// session, so no connected server may take it.
const BuiltinID = "repogo"

var builtins = []Builtin{{
	ID:          BuiltinID,
	Label:       "RepoGo",
	Description: "RepoGo's built-in MCP server.",
}}

type file struct {
	Servers []record `json:"servers"`
	// The built-ins switched off on this environment; the rest are on.
	BuiltinsOff []string `json:"builtins_off,omitempty"`
}

type Service struct {
	path      string
	providers []Provider
	http      *http.Client

	mu       sync.Mutex
	servers  []record
	off      []string
	pending  map[string]*pending
	onChange func(Changed)

	// One refresh per server at a time: providers that rotate refresh tokens
	// invalidate the first result when a second lands.
	refreshing sync.Mutex
}

// Open loads path, 0600 because it holds the users' credentials for other
// services. changed is told the whole list after every change, for mcp.changed.
func Open(path string, changed func(Changed), providers ...Provider) (*Service, error) {
	s := &Service{
		path:      path,
		onChange:  changed,
		providers: providers,
		http:      &http.Client{Timeout: 15 * time.Second},
		pending:   map[string]*pending{},
	}
	var f file
	if _, err := apphome.ReadJSON(path, &f); err != nil {
		return nil, err
	}
	s.servers, s.off = f.Servers, f.BuiltinsOff
	return s, nil
}

// Listing is every connected server, by label, and the built-ins. Given a
// project, it also has the agents' own servers there, and Root is the project
// a switch lands on.
type Listing struct {
	Servers   []Server
	Builtins  []Builtin
	Installed []Installed
	Root      string
}

func (s *Service) List(project string) Listing {
	var root string
	if strings.TrimSpace(project) != "" {
		root = Root(project)
	}
	s.mu.Lock()
	servers, on := s.publicLocked(), s.builtinsLocked()
	s.mu.Unlock()
	return Listing{Servers: servers, Builtins: on, Installed: s.installed(root), Root: root}
}

func (s *Service) builtinsLocked() []Builtin {
	out := make([]Builtin, 0, len(builtins))
	for _, b := range builtins {
		b.On = !slices.Contains(s.off, b.ID)
		out = append(out, b)
	}
	return out
}

// BuiltinOn reports whether a built-in is on: everywhere on this environment
// unless switched off.
func (s *Service) BuiltinOn(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !slices.Contains(s.off, id)
}

func isBuiltin(id string) bool {
	return slices.ContainsFunc(builtins, func(b Builtin) bool { return b.ID == id })
}

func (s *Service) publicLocked() []Server {
	out := make([]Server, 0, len(s.servers))
	for _, r := range s.servers {
		srv := r.Server
		srv.Projects = slices.Clone(srv.Projects)
		if srv.Projects == nil {
			srv.Projects = []string{}
		}
		out = append(out, srv)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Label) < strings.ToLower(out[j].Label) })
	return out
}

func (s *Service) Rename(id, label string) error {
	if isBuiltin(id) {
		return ErrInvalid.Errorf("a built-in server keeps its name")
	}
	label = strings.TrimSpace(label)
	if label == "" {
		return ErrInvalid.Errorf("a name is required")
	}
	return s.update(id, func(r *record) { r.Label = label })
}

// SetEnabled switches a connected server on or off for one project, any
// folder inside it; the switch lands on its root. A built-in switches for the
// whole environment, so it ignores project.
func (s *Service) SetEnabled(id, project string, enabled bool) error {
	if isBuiltin(id) {
		return s.commit(func() error {
			s.off = slices.DeleteFunc(slices.Clone(s.off), func(off string) bool { return off == id })
			if !enabled {
				s.off = append(s.off, id)
			}
			return nil
		})
	}
	if strings.TrimSpace(project) == "" {
		return ErrInvalid.Errorf("project is required")
	}
	root := Root(project)
	return s.update(id, func(r *record) {
		// A copy: the rollback in commit still holds the old array.
		r.Projects = switched(r.Projects, root, enabled)
	})
}

// switched is projects with root on or off. A copy: the rollback in commit
// still holds the old array.
func switched(projects []string, root string, enabled bool) []string {
	out := slices.DeleteFunc(slices.Clone(projects), func(p string) bool { return p == root })
	if enabled {
		out = append(out, root)
	}
	return out
}

func (s *Service) Remove(id string) error {
	if isBuiltin(id) {
		return ErrInvalid.Errorf("a built-in server cannot be removed; switch it off instead")
	}
	return s.commit(func() error {
		i := s.indexLocked(id)
		if i < 0 {
			return ErrNotFound
		}
		s.servers = slices.Delete(s.servers, i, i+1)
		return nil
	})
}

func (s *Service) update(id string, fn func(*record)) error {
	return s.commit(func() error {
		i := s.indexLocked(id)
		if i < 0 {
			return ErrNotFound
		}
		fn(&s.servers[i])
		return nil
	})
}

// put adds a connection, or replaces the one for the same URL whole:
// re-adding is reconnecting, so nothing of the old credential may linger.
func (s *Service) put(r record) (Server, error) {
	return r.Server, s.commit(func() error {
		if i := s.indexLocked(r.ID); i >= 0 {
			s.servers[i] = r
		} else {
			s.servers = append(s.servers, r)
		}
		return nil
	})
}

func (s *Service) indexLocked(id string) int {
	return slices.IndexFunc(s.servers, func(r record) bool { return r.ID == id })
}

// commit applies change under the lock, writes the file, and announces the
// list; a failed write restores the servers, so memory never runs ahead of disk.
func (s *Service) commit(change func() error) error {
	s.mu.Lock()
	before, beforeOff := slices.Clone(s.servers), s.off
	err := change()
	if err == nil {
		if err = apphome.WriteJSON(s.path, file{Servers: s.servers, BuiltinsOff: s.off}, 0o600); err != nil {
			s.servers, s.off = before, beforeOff
		}
	}
	list, on := s.publicLocked(), s.builtinsLocked()
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.onChange(Changed{Servers: list, Builtins: on})
	return nil
}

// serverID is a function of the URL, so re-adding one updates it in place and
// a chat's live agent, which captured the old entry, keeps the same name.
func serverID(u string) string {
	sum := sha256.Sum256([]byte(u))
	return hex.EncodeToString(sum[:8])
}

// normalizeURL accepts what a person pastes: a bare host gets https. Plain
// http is allowed because this host is the user's own machine, where a local
// MCP server is a normal thing to run.
func normalizeURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ErrInvalid.Errorf("a URL is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", ErrInvalid.Errorf("that is not a valid URL")
	}
	if u.User != nil {
		return "", ErrInvalid.Errorf("MCP URLs cannot carry credentials")
	}
	u.Fragment = ""
	return u.String(), nil
}

func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil && p.Hostname() != "" {
		return p.Hostname()
	}
	return u
}
