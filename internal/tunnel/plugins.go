package tunnel

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/repogoapp/host/tunnelwire"
)

// ErrNoRoute is a plugin route that doesn't exist or isn't open to HTTP. Both
// answer the same 404, so a caller can't learn which plugins are installed.
var ErrNoRoute = errors.New("no such plugin route")

// Plugins answers requests to the environment's own address. It decides
// which methods are open to HTTP and who may call them.
type Plugins interface {
	// HasRoutes reports whether any plugin serves HTTP; while one does, the
	// host keeps its gateway socket open.
	HasRoutes() bool
	// ServeHTTP answers req for route, or ErrNoRoute.
	ServeHTTP(ctx context.Context, route tunnelwire.PluginRoute, req *http.Request) (*http.Response, error)
}

// envHost is this host's own public name, h-<host id>.<Domain>.
func (s *Service) envHost() string {
	return tunnelwire.EnvLabel(string(s.cfg.Identity.ID)) + "." + Domain
}

func (l *link) servePlugin(ctx context.Context, req *tunnelwire.PluginRequest) {
	id := req.SessionID
	if id == "" {
		return
	}
	notFound := func() { l.sendError(id, http.StatusNotFound, "Nothing is served at this address.", "Not found") }
	// The gateway names the address it was asked for; only this host's own counts.
	public := strings.ToLower(headerValue(req.HeaderLines, "host"))
	if public != l.s.envHost() || l.s.cfg.Plugins == nil {
		notFound()
		return
	}
	path, ok := requestPath(req.Path)
	if !ok {
		notFound()
		return
	}
	pathOnly, _, _ := strings.Cut(path, "?")
	route, ok := tunnelwire.ParsePluginPath(pathOnly)
	if !ok {
		notFound()
		return
	}
	method := strings.ToUpper(req.Method)
	if method == "" {
		method = http.MethodGet
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, "https://"+public+path, bytes.NewReader(req.Body))
	if err != nil {
		notFound()
		return
	}
	copyHeaders(httpReq.Header, req.HeaderLines, nil)
	forwarded(httpReq.Header, public)

	resp, err := l.s.cfg.Plugins.ServeHTTP(ctx, route, httpReq)
	switch {
	case errors.Is(err, ErrNoRoute):
		notFound()
	case err != nil:
		l.s.cfg.Log.Warn("tunnel: plugin route failed", "plugin", route.Plugin, "method", route.Method, "err", err)
		l.sendError(id, http.StatusBadGateway, "The plugin couldn't answer this request.", "Plugin error")
	default:
		l.relay(id, method, resp, false)
	}
}
