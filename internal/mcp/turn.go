package mcp

import (
	"context"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/files"
	"github.com/repogo/host/internal/git"
)

// Refresh this long before expiry, so a token does not lapse mid-turn.
const refreshSkew = 5 * time.Minute

// ForTurn is the servers switched on for cwd's project, ready for a turn. One
// that cannot be called (OAuth with no token) is left out, not failed: MCP is
// never the reason a turn cannot run.
func (s *Service) ForTurn(ctx context.Context, cwd string) []agent.MCPServer {
	root := Root(cwd)
	s.mu.Lock()
	var on []record
	for _, r := range s.servers {
		// A folder outside git has no root above it, so a chat in one of its
		// subfolders still belongs to the project it was switched on in.
		if slices.ContainsFunc(r.Projects, func(p string) bool { return files.Within(p, root) }) {
			on = append(on, r)
		}
	}
	s.mu.Unlock()

	// RepoGo's own server is added by the agent, under its own name.
	taken := map[string]bool{BuiltinID: true}
	var out []agent.MCPServer
	for _, r := range on {
		if r.OAuth != nil && r.Refresh != "" && r.ExpiresAt != 0 &&
			time.Until(time.UnixMilli(r.ExpiresAt)) < refreshSkew {
			r = s.refresh(ctx, r.ID)
		}
		headers := []agent.Header{}
		switch {
		case r.Auth != AuthNone && r.Secret == "":
			continue
		case r.HeaderName != "":
			headers = append(headers, agent.Header{Name: r.HeaderName, Value: r.Secret})
		case r.Secret != "":
			headers = append(headers, agent.Header{Name: "Authorization", Value: "Bearer " + r.Secret})
		}
		key := slug(r.Label, taken)
		taken[key] = true
		out = append(out, agent.MCPServer{Type: "http", Name: key, URL: r.URL, Headers: headers})
	}
	return out
}

// refresh trades the stored refresh token for a new access token and returns
// the record as it now stands. Soft: on failure the old token is kept, which
// may still work, and a dead one surfaces as the server's own 401.
func (s *Service) refresh(ctx context.Context, id string) record {
	s.refreshing.Lock()
	defer s.refreshing.Unlock()

	s.mu.Lock()
	i := s.indexLocked(id)
	if i < 0 {
		s.mu.Unlock()
		return record{}
	}
	r := s.servers[i]
	s.mu.Unlock()
	// Another turn refreshed it while this one waited.
	if r.ExpiresAt == 0 || time.Until(time.UnixMilli(r.ExpiresAt)) >= refreshSkew {
		return r
	}

	g, err := s.tokenCall(ctx, r.OAuth, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {r.Refresh},
		"client_id":     {r.OAuth.ClientID},
	})
	if err != nil {
		return r
	}
	var out record
	err = s.update(id, func(stored *record) {
		stored.Secret, stored.ExpiresAt = g.access, g.expiresAt
		// A provider that rotates invalidates the old refresh token.
		if g.refresh != "" {
			stored.Refresh = g.refresh
		}
		out = *stored
	})
	if err != nil {
		return r
	}
	return out
}

// Root is the project a folder belongs to: the top of its git checkout, and
// for a linked worktree the main checkout, so a switch set in a project holds
// in every worktree a chat branches into. A folder outside git is its own.
func Root(dir string) string {
	if root := git.Root(dir); root != "" {
		return git.MainCheckout(root)
	}
	return filepath.Clean(dir)
}

// slug is the key an agent namespaces the server's tools under
// (`mcp__<key>__<tool>`), made unique within one turn.
func slug(label string, taken map[string]bool) string {
	var b strings.Builder
	sep := false
	for _, r := range strings.ToLower(label) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			b.WriteRune(r)
			sep = false
		} else if b.Len() > 0 && !sep {
			b.WriteByte('_')
			sep = true
		}
	}
	base := strings.Trim(b.String(), "_")
	if base == "" {
		base = "server"
	}
	key := base
	for n := 2; taken[key]; n++ {
		key = base + "_" + strconv.Itoa(n)
	}
	return key
}
