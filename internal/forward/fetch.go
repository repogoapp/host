package forward

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

const fetchTimeout = 30 * time.Second

// Request is one GET to a dev server's path.
type Request struct {
	Port int    `json:"port"`
	Path string `json:"path"`
}

// Response is the dev server's status; the one caller, a sign-in callback,
// reads nothing else.
type Response struct {
	Status int `json:"status"`
}

// Fetcher sends one request to loopback, for a caller that wants the answer
// rather than a socket (a CLI's localhost OAuth callback).
type Fetcher struct {
	client *http.Client
	// Logged per request: "the sign-in hung" looks identical whichever hop failed.
	log *slog.Logger
}

func NewFetcher(log *slog.Logger) *Fetcher {
	return &Fetcher{client: Loopback(fetchTimeout), log: log}
}

// Fetch sends one request, logging the outcome.
func (f *Fetcher) Fetch(ctx context.Context, req Request) (Response, error) {
	resp, err := f.fetch(ctx, req)
	if err != nil {
		f.log.Warn("forward: fetch failed", "port", req.Port, "path", req.Path, "err", err)
		return Response{}, err
	}
	f.log.Info("forward: fetch", "port", req.Port, "path", req.Path, "status", resp.Status)
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
	url := "http://127.0.0.1:" + strconv.Itoa(req.Port) + path
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Response{}, fmt.Errorf("forward: build request: %w", err)
	}
	resp, err := f.client.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("forward: fetch %s: %w", url, err)
	}
	resp.Body.Close()
	return Response{Status: resp.StatusCode}, nil
}
