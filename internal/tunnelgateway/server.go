package tunnelgateway

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/repogoapp/host/tunnelwire"
)

type Server struct {
	// Domain is where tunnels live: `<slug>.<Domain>`.
	Domain string
	// GatewayID is the name hosts connect to, signed into every host proof so
	// a proof made for this gateway is useless at another. It is never a slug.
	GatewayID string
	Registry  *Upstash
	Hub       *Hub
	Cache     *ResponseCache
	Limits    *RateLimits
	Log       *slog.Logger
	Now       func() time.Time

	bodies  bodyBudget
	sockets socketCounts
	misses  missCache
}

// maxRequestBody caps a visitor's request body, which crosses in one frame.
const maxRequestBody = 32 << 20

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(s.serve)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	slug, ok := s.slug(host)
	if !ok || host == s.GatewayID {
		s.serveOwn(w, r)
		return
	}
	if s.Limits != nil && !s.Limits.AllowIP(clientIP(r), s.now()) {
		writeError(w, r, http.StatusTooManyRequests, "Too many requests", "You're sending more requests than this gateway takes. Try again in a moment.")
		return
	}
	if strings.HasPrefix(slug, tunnelwire.EnvPrefix) {
		s.serveEnv(w, r, slug)
		return
	}
	row, h, ok := s.resolve(w, r, slug)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	if isWebSocketUpgrade(r) {
		s.serveWebSocket(w, r, row, h)
		return
	}
	s.serveHTTP(w, r, row, h)
}

// serveOwn answers every name that isn't a tunnel: the host socket, which the
// key proof guards whatever name it came in on, and a health check.
func (s *Server) serveOwn(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case tunnelwire.ConnectPath:
		s.serveConnect(w, r)
	case "/healthz":
		_, _ = w.Write([]byte("ok"))
	default:
		writeError(w, r, http.StatusNotFound, "Not a tunnel", "This address isn't a RepoGo tunnel. Check the link.")
	}
}

// resolve finds the host behind slug: a live row, a bare port, within its
// rate limits, and connected with the key the row names.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request, slug string) (*Tunnel, *Host, bool) {
	now := s.now()
	if !tunnelwire.IsSlug(slug) || s.misses.recent(slug, now) {
		writeError(w, r, http.StatusNotFound, "Tunnel not found", "No tunnel is open at this address. The owner may have closed it.")
		return nil, nil, false
	}
	row, err := s.Registry.Lookup(r.Context(), slug)
	if err != nil {
		s.Log.Warn("registry lookup", "slug", slug, "err", err)
		writeError(w, r, http.StatusBadGateway, "Lookup failed", "Couldn't look up which computer serves this link. Try again in a moment.")
		return nil, nil, false
	}
	switch {
	case row == nil:
		s.misses.add(slug, now)
		writeError(w, r, http.StatusNotFound, "Tunnel not found", "No tunnel is open at this address. The owner may have closed it.")
	case row.HostID == "":
		writeError(w, r, http.StatusNotFound, "Tunnel not found", "No tunnel is open at this address. The owner may have closed it.")
	case !row.Enabled:
		writeError(w, r, http.StatusServiceUnavailable, "Tunnel disabled", "This tunnel is turned off. The owner can turn it back on in RepoGo.")
	case row.ExpiresAt <= now.UnixMilli():
		writeError(w, r, http.StatusGone, "Tunnel expired", "This tunnel has expired. The owner can open a new one in RepoGo.")
	case !isPort(row.Target):
		writeError(w, r, http.StatusBadGateway, "Invalid target", "This tunnel was opened for something that isn't a port.")
	case s.Limits != nil && !s.Limits.AllowSlug(slug, now):
		writeError(w, r, http.StatusTooManyRequests, "Too many requests", "This tunnel is getting more requests than it can take. Try again in a moment.")
	default:
		h, err := s.Hub.Lookup(row.HostID)
		if err != nil {
			writeError(w, r, http.StatusBadGateway, "Computer unreachable", "The computer serving this tunnel isn't connected right now. Check that RepoGo is running on it.")
			return nil, nil, false
		}
		return row, h, true
	}
	return nil, nil, false
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// slug is the single label in front of the domain; deeper names and IDNs are
// never tunnels.
func (s *Server) slug(host string) (string, bool) {
	slug, ok := strings.CutSuffix(host, "."+s.Domain)
	if !ok || slug == "" || strings.Contains(slug, ".") || strings.HasPrefix(slug, "xn--") {
		return "", false
	}
	return slug, true
}

// hostLine names the public host on a forwarded request, so the host can check
// the slug against the tunnels it opened. Visitors' own Host never reaches
// flattenHeaders, so they cannot supply this line.
func hostLine(r *http.Request) string {
	return "Host: " + strings.ToLower(r.Host)
}

// isPort accepts a bare unprivileged port, the only target a tunnel may name.
func isPort(target string) bool {
	if target == "" || strings.TrimLeft(target, "0123456789") != "" || target[0] == '0' {
		return false
	}
	n, err := strconv.Atoi(target)
	return err == nil && n >= 1024 && n <= 65535
}

func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		if strings.Contains(strings.ToLower(v), "upgrade") {
			return true
		}
	}
	return false
}

func newSessionID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
