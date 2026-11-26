package tunnelgateway

import (
	"errors"
	"net/http"

	"github.com/repogoapp/host/tunnelwire"
)

// An environment's own address, h-<host id>.<domain>, reaches only the plugin
// routes its host serves. It needs no registry row: the id is the one the
// host proved, so the name is stable for as long as the host keeps its key.

// maxPluginBody caps a request to a plugin: webhooks and API calls, not uploads.
const maxPluginBody = 1 << 20

func (s *Server) serveEnv(w http.ResponseWriter, r *http.Request, label string) {
	id, ok := tunnelwire.EnvHostID(label)
	if !ok || isWebSocketUpgrade(r) {
		writeError(w, r, http.StatusNotFound, "Not found", "Nothing is served at this address.")
		return
	}
	if _, ok := tunnelwire.ParsePluginPath(r.URL.Path); !ok {
		writeError(w, r, http.StatusNotFound, "Not found", "Nothing is served at this address.")
		return
	}
	if s.Limits != nil && !s.Limits.AllowSlug(label, s.now()) {
		writeError(w, r, http.StatusTooManyRequests, "Too many requests", "This address is getting more requests than it can take. Try again in a moment.")
		return
	}
	// Offline answers as a missing method or key does, so the address
	// doesn't tell a stranger when its computer is on.
	h, err := s.Hub.Lookup(id)
	if err != nil {
		writeError(w, r, http.StatusNotFound, "Not found", "Nothing is served at this address.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPluginBody)
	body, release, err := s.readBody(w, r)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, r, http.StatusRequestEntityTooLarge, "Request too large", "This address accepts request bodies up to 1 MB.")
		return
	}
	if errors.Is(err, errBodyBudget) {
		writeError(w, r, http.StatusServiceUnavailable, "Gateway busy", "The gateway is taking as many requests as it can. Try again in a moment.")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "Couldn't read request", "The gateway failed to read your request body. Try again.")
		return
	}

	session := newSessionID()
	result, err := h.DoPlugin(r.Context(), &tunnelwire.PluginRequest{
		SessionID:   session,
		Method:      r.Method,
		Path:        r.URL.RequestURI(),
		HeaderLines: append(flattenHeaders(r.Header), hostLine(r)),
		Body:        body,
	})
	release()
	if err != nil {
		status, title, msg := s.hostError(err)
		writeError(w, r, status, title, msg)
		return
	}
	resp := result.Response
	ready := writeDeadline(w)
	if title, ok := extractGatewayErrorTitle(resp.HeaderLines); ok {
		writeError(w, r, resp.Status, title, string(resp.Body))
		return
	}
	// Never cached or re-encoded: answers depend on keys and tokens, and a
	// plugin's bytes reach the caller as it wrote them.
	writeResponseHeaders(w, stripCookieDomain(resp.HeaderLines))
	w.Header().Set("Cache-Control", "no-store")
	ready()
	w.WriteHeader(resp.Status)
	if !resp.Streaming {
		_, _ = w.Write(resp.Body)
		return
	}
	defer h.FinishHTTP(session)
	flusher, _ := w.(http.Flusher)
	for {
		select {
		case <-r.Context().Done():
			return
		case c, ok := <-result.Chunks:
			if !ok {
				return
			}
			ready()
			if _, err := w.Write(c.Data); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			if c.EndOfStream {
				return
			}
		}
	}
}
