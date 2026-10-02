package forward

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

// MaxBodyBytes stays below the 8 MB message limit: base64 costs a third on
// top, and a body that fails after encoding fails as a serialization error.
const MaxBodyBytes = 5 << 20

const fetchTimeout = 30 * time.Second

// Request is one buffered proxy call.
type Request struct {
	Port    int                 `json:"port"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

// Response is what the dev server said.
type Response struct {
	Status  int                 `json:"status"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    []byte              `json:"body,omitempty"`

	// Truncated means the body hit MaxBodyBytes. Reported rather than silently
	// cut: a half-delivered asset that claims success is worse than an error.
	Truncated bool `json:"truncated,omitempty"`
}

// Fetcher proxies one buffered request to loopback, for a caller that wants the
// answer rather than a socket (a CLI's localhost OAuth callback). One client,
// so connections are pooled across requests.
type Fetcher struct {
	client *http.Client
	// Logged per request: "the page is blank" looks identical whichever hop failed.
	log *slog.Logger
}

func NewFetcher(log *slog.Logger) *Fetcher {
	return &Fetcher{client: Loopback(fetchTimeout), log: log}
}

// Fetch proxies one request, logging the outcome.
func (f *Fetcher) Fetch(ctx context.Context, req Request) (Response, error) {
	resp, err := f.fetch(ctx, req)
	if err != nil {
		f.log.Warn("forward: fetch failed", "port", req.Port, "path", req.Path, "err", err)
		return Response{}, err
	}
	// Debug: one page load is sixty assets. Failures stay loud.
	f.log.Debug("forward: fetch", "port", req.Port, "path", req.Path,
		"status", resp.Status, "bytes", len(resp.Body))
	return resp, nil
}

func (f *Fetcher) fetch(ctx context.Context, req Request) (Response, error) {
	if req.Port <= 0 || req.Port > 65535 {
		return Response{}, ErrPort
	}
	path := req.Path
	if path == "" || path[0] != '/' {
		path = "/" + path
	}
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}

	url := "http://127.0.0.1:" + strconv.Itoa(req.Port) + path
	httpReq, err := http.NewRequestWithContext(ctx, method, url, bodyReader(req.Body))
	if err != nil {
		return Response{}, fmt.Errorf("forward: build request: %w", err)
	}
	for name, values := range req.Headers {
		if HopByHop(name) {
			continue
		}
		for _, v := range values {
			httpReq.Header.Add(name, v)
		}
	}
	// Go derives Host from the URL, so a caller's own Host header is dropped by
	// net/http unless set here. Dev servers route on it — a framework serving
	// several sites off one port needs the name the browser used.
	if host := firstHeader(req.Headers, "Host"); host != "" {
		httpReq.Host = host
	}

	resp, err := f.client.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("forward: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()

	// One byte past the cap, so a body exactly at the limit is not reported as
	// truncated.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		return Response{}, fmt.Errorf("forward: read body: %w", err)
	}
	out := Response{Status: resp.StatusCode, Headers: map[string][]string{}}
	for name, values := range resp.Header {
		if HopByHop(name) {
			continue
		}
		out.Headers[name] = values
	}
	if len(body) > MaxBodyBytes {
		out.Body, out.Truncated = body[:MaxBodyBytes], true
	} else {
		out.Body = body
	}
	return out, nil
}

func bodyReader(b []byte) io.Reader {
	if len(b) == 0 {
		return nil
	}
	return bytes.NewReader(b)
}

func firstHeader(headers map[string][]string, name string) string {
	for key, values := range headers {
		if http.CanonicalHeaderKey(key) == name && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}
