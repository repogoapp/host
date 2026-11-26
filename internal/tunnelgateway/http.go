package tunnelgateway

import (
	"compress/gzip"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/repogoapp/host/tunnelwire"
)

// Dropped both ways: hop-by-hop headers, plus host, content-length and
// content-encoding, which the gateway sets itself after it may re-encode.
var hopByHop = map[string]bool{
	"host": true, "connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailer": true, "transfer-encoding": true,
	"upgrade": true, "content-length": true, "content-encoding": true,
}

// serveHTTP forwards one request to the host. The host answers in one
// Response, or with Streaming set and BodyChunks after it.
func (s *Server) serveHTTP(w http.ResponseWriter, r *http.Request, row *Tunnel, h *Host) {
	cacheKey, cacheable := s.cacheKey(r, row)
	if cacheable {
		if entry, ok := s.Cache.Get(cacheKey); ok {
			if entry.matchesValidators(r) {
				writeCachedResponse(w, r, entry, http.StatusNotModified, "revalidated")
				return
			}
			if entry.canServeBody(r) {
				writeCachedResponse(w, r, entry, entry.status, "hit")
				return
			}
		}
	}

	body, release, err := s.readBody(w, r)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, r, http.StatusRequestEntityTooLarge, "Request too large", "This tunnel accepts request bodies up to 32 MB.")
		return
	}
	if errors.Is(err, errBodyBudget) {
		writeError(w, r, http.StatusServiceUnavailable, "Gateway busy", "The gateway is taking as many uploads as it can. Try again in a moment.")
		return
	}
	if err != nil {
		writeError(w, r, http.StatusBadGateway, "Couldn't read request", "The gateway failed to read your request body. Try again.")
		return
	}

	id := newSessionID()
	result, err := h.DoHTTP(r.Context(), &tunnelwire.Request{
		SessionID:   id,
		Method:      r.Method,
		Path:        r.URL.RequestURI(),
		Target:      row.Target,
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
	resp.HeaderLines = stripCookieDomain(resp.HeaderLines)
	if title, ok := extractGatewayErrorTitle(resp.HeaderLines); ok {
		writeError(w, r, resp.Status, title, string(resp.Body))
		return
	}

	writeResponseHeaders(w, resp.HeaderLines)
	encoding := responseEncoding(r, resp.Status, resp.HeaderLines)
	if encoding != "" {
		addEncodingHeaders(w, encoding)
	}
	ready()
	w.WriteHeader(resp.Status)

	if !resp.Streaming {
		writeBody(w, resp.Body, encoding)
		if cacheable && r.Method == http.MethodGet {
			s.storeCachedResponse(cacheKey, r, resp, resp.Body)
		}
		return
	}

	defer h.FinishHTTP(id)
	flusher, _ := w.(http.Flusher)
	bodyWriter, encoder := responseBodyWriter(w, encoding)
	if encoder != nil {
		defer encoder.Close()
	}
	cacheBody, collect := s.newStreamingCacheBuffer(cacheable, r, resp)
	// Headers go out now so an EventSource starts before the first event.
	if flusher != nil {
		flusher.Flush()
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case c, ok := <-result.Chunks:
			if !ok {
				return
			}
			ready()
			if len(c.Data) > 0 {
				if _, err := bodyWriter.Write(c.Data); err != nil {
					return
				}
				if encoder != nil {
					_ = encoder.Flush()
				}
				if flusher != nil {
					flusher.Flush()
				}
				if collect {
					if len(cacheBody)+len(c.Data) <= s.Cache.MaxItemBytes() {
						cacheBody = append(cacheBody, c.Data...)
					} else {
						cacheBody, collect = nil, false
					}
				}
			}
			if c.EndOfStream {
				if collect {
					s.storeCachedResponse(cacheKey, r, resp, cacheBody)
				}
				return
			}
		}
	}
}

// visitorWriteTimeout bounds one write to a visitor, reset before each, so a
// visitor that stops reading frees its session while a quiet stream lives on.
const visitorWriteTimeout = 60 * time.Second

// writeDeadline returns what to call before each write to w.
func writeDeadline(w http.ResponseWriter) func() {
	rc := http.NewResponseController(w)
	return func() { _ = rc.SetWriteDeadline(time.Now().Add(visitorWriteTimeout)) }
}

func (s *Server) newStreamingCacheBuffer(cacheable bool, r *http.Request, resp *tunnelwire.Response) ([]byte, bool) {
	if !cacheable || r.Method != http.MethodGet || resp.Status != http.StatusOK {
		return nil, false
	}
	if !responseAllowsCache(resp.HeaderLines) || responseBodyFreshDuration(r.URL.Path, resp.HeaderLines) <= 0 {
		return nil, false
	}
	return make([]byte, 0, min(s.Cache.MaxItemBytes(), 1<<20)), true
}

func (s *Server) storeCachedResponse(key string, r *http.Request, resp *tunnelwire.Response, body []byte) {
	if resp.Status != http.StatusOK || len(body) == 0 || !responseAllowsCache(resp.HeaderLines) {
		return
	}
	s.Cache.Set(key, resp.Status, resp.HeaderLines, body, responseBodyFreshDuration(r.URL.Path, resp.HeaderLines), r.Header)
}

// cacheKey is set only for requests every visitor would get the same answer to.
func (s *Server) cacheKey(r *http.Request, row *Tunnel) (string, bool) {
	if s.Cache == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return "", false
	}
	if r.Header.Get("Range") != "" || carriesCredential(r.Header) {
		return "", false
	}
	parts := []string{row.HostID, row.Target, strings.ToLower(r.Host), r.URL.RequestURI(), "accept=" + r.Header.Get("Accept")}
	return strings.Join(parts, "\x00"), true
}

// carriesCredential is whether a request may be answered per caller: a cookie,
// or a header named like a key or token (Authorization, X-API-Key, apikey).
func carriesCredential(h http.Header) bool {
	for name := range h {
		name = strings.ToLower(name)
		if name == "cookie" || strings.Contains(name, "auth") || strings.Contains(name, "key") || strings.Contains(name, "token") {
			return true
		}
	}
	return false
}

// isStaticAsset is a path cached for a short while even without cache
// headers, so a dev server's assets load fast. JSON is left out: it is
// usually an answer that differs per caller.
func isStaticAsset(path string) bool {
	if strings.HasPrefix(path, "/_next/static/") || path == "/_next/image" {
		return true
	}
	for _, ext := range []string{".avif", ".css", ".gif", ".ico", ".jpeg", ".jpg", ".js", ".map", ".mjs",
		".mp4", ".otf", ".png", ".svg", ".ttf", ".wasm", ".webm", ".webp", ".woff", ".woff2"} {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

func writeCachedResponse(w http.ResponseWriter, r *http.Request, entry *cacheEntry, status int, cacheStatus string) {
	w.Header().Set("x-repogo-tunnel-cache", cacheStatus)
	writeResponseHeaders(w, entry.headerLines)
	encoding := responseEncoding(r, status, entry.headerLines)
	if encoding != "" {
		addEncodingHeaders(w, encoding)
	}
	w.WriteHeader(status)
	if status == http.StatusNotModified || r.Method == http.MethodHead {
		return
	}
	writeBody(w, entry.body, encoding)
}

func writeBody(w http.ResponseWriter, body []byte, encoding string) {
	if len(body) == 0 {
		return
	}
	bodyWriter, encoder := responseBodyWriter(w, encoding)
	if encoder != nil {
		defer encoder.Close()
	}
	_, _ = bodyWriter.Write(body)
}

// bodyEncoder flushes per chunk, so streamed events aren't held back.
type bodyEncoder interface {
	io.WriteCloser
	Flush() error
}

func responseBodyWriter(w http.ResponseWriter, encoding string) (io.Writer, bodyEncoder) {
	switch encoding {
	case "br":
		// Level 4 costs about what gzip does and is about a third smaller.
		enc := brotli.NewWriterLevel(w, 4)
		return enc, enc
	case "gzip":
		enc, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			return w, nil
		}
		return enc, enc
	}
	return w, nil
}

// responseEncoding is the encoding the gateway applies on the way out: br when
// the visitor accepts it, else gzip, else none.
func responseEncoding(r *http.Request, status int, headerLines []string) string {
	if r.Method == http.MethodHead || r.Header.Get("Range") != "" {
		return ""
	}
	if status == http.StatusNoContent || status == http.StatusNotModified || status < 200 {
		return ""
	}
	if headerLineValue(headerLines, "content-encoding") != "" {
		return ""
	}
	if strings.Contains(strings.ToLower(headerLineValue(headerLines, "cache-control")), "no-transform") {
		return ""
	}
	contentType := strings.ToLower(headerLineValue(headerLines, "content-type"))
	compressible := isCompressiblePath(strings.ToLower(r.URL.Path))
	if contentType != "" {
		compressible = isCompressibleContentType(contentType)
	}
	if !compressible {
		return ""
	}
	accept := r.Header.Get("Accept-Encoding")
	for _, candidate := range []string{"br", "gzip"} {
		if clientAcceptsEncoding(accept, candidate) {
			return candidate
		}
	}
	return ""
}

func clientAcceptsEncoding(header, encoding string) bool {
	for _, part := range strings.Split(header, ",") {
		tokens := strings.Split(strings.TrimSpace(strings.ToLower(part)), ";")
		if tokens[0] != encoding {
			continue
		}
		for _, param := range tokens[1:] {
			q, ok := strings.CutPrefix(strings.TrimSpace(param), "q=")
			if ok {
				return q != "0" && !strings.HasPrefix(q, "0.0")
			}
		}
		return true
	}
	return false
}

func isCompressibleContentType(contentType string) bool {
	return strings.HasPrefix(contentType, "text/") ||
		strings.Contains(contentType, "javascript") ||
		strings.Contains(contentType, "json") ||
		strings.Contains(contentType, "xml") ||
		strings.Contains(contentType, "svg")
}

func isCompressiblePath(path string) bool {
	for _, ext := range []string{".css", ".js", ".json", ".map", ".mjs", ".svg", ".txt", ".wasm"} {
		if strings.HasSuffix(path, ext) {
			return true
		}
	}
	return false
}

func addEncodingHeaders(w http.ResponseWriter, encoding string) {
	w.Header().Set("Content-Encoding", encoding)
	for _, existing := range w.Header().Values("Vary") {
		for _, part := range strings.Split(existing, ",") {
			if strings.EqualFold(strings.TrimSpace(part), "Accept-Encoding") {
				return
			}
		}
	}
	w.Header().Add("Vary", "Accept-Encoding")
}

// hostError is the visitor's page for err; anything unexpected is logged, not
// shown, since its text may name the gateway's own addresses.
func (s *Server) hostError(err error) (int, string, string) {
	switch {
	case errors.Is(err, ErrHostClosed):
		return http.StatusBadGateway, "Computer disconnected", "The computer serving this tunnel disconnected mid-request. Try again."
	case errors.Is(err, ErrNoHost):
		return http.StatusBadGateway, "Computer unreachable", "The computer serving this tunnel isn't connected right now."
	case errors.Is(err, ErrHostBusy):
		return http.StatusServiceUnavailable, "Computer busy", "The computer serving this tunnel is answering as many requests as it can. Try again in a moment."
	}
	s.Log.Warn("request failed", "err", err)
	return http.StatusBadGateway, "Tunnel error", "The gateway couldn't complete the request. Try again."
}

func flattenHeaders(h http.Header) []string {
	out := make([]string, 0, len(h)+1)
	for k, vs := range h {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			out = append(out, k+": "+v)
		}
	}
	return out
}

// writeResponseHeaders writes a host's headers for the visitor. Fly's proxy
// acts on fly-* headers and on a replay content type, sending the request on
// to another app, so a host may set neither.
func writeResponseHeaders(w http.ResponseWriter, lines []string) {
	for _, line := range lines {
		k, v, ok := strings.Cut(line, ": ")
		lower := strings.ToLower(k)
		if !ok || k == "" || hopByHop[lower] || strings.HasPrefix(lower, "fly-") {
			continue
		}
		if lower == "content-type" && strings.Contains(strings.ToLower(v), "fly.replay") {
			v = "application/octet-stream"
		}
		w.Header().Add(k, v)
	}
}

// stripCookieDomain drops Domain= from Set-Cookie, so one tunnel cannot set a
// cookie that every other tunnel under the domain receives.
func stripCookieDomain(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		key, value, ok := strings.Cut(line, ": ")
		if !ok || !strings.EqualFold(key, "set-cookie") {
			out = append(out, line)
			continue
		}
		parts := strings.Split(value, ";")
		kept := parts[:1]
		for _, p := range parts[1:] {
			name, _, _ := strings.Cut(strings.TrimSpace(p), "=")
			if !strings.EqualFold(name, "domain") {
				kept = append(kept, p)
			}
		}
		out = append(out, key+": "+strings.Join(kept, ";"))
	}
	return out
}
