package tunnelgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Tunnel is the row behind `<slug>.<domain>`. A row without a HostID names no
// host to serve it, so it is never served.
type Tunnel struct {
	HostID    string `json:"hostId"`
	Target    string `json:"target"` // the host's local port
	Enabled   bool   `json:"enabled"`
	ExpiresAt int64  `json:"expiresAt"` // Unix ms
}

type Upstash struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func NewUpstash(baseURL, token string) *Upstash {
	return &Upstash{BaseURL: baseURL, Token: token, HTTP: &http.Client{Timeout: 5 * time.Second}}
}

// Lookup is the row for slug, or nil when there is none. It reads Upstash on
// every request, so a closed link stops at once.
func (u *Upstash) Lookup(ctx context.Context, slug string) (*Tunnel, error) {
	return u.get(ctx, "tunnel:"+slug)
}

func (u *Upstash) get(ctx context.Context, key string) (*Tunnel, error) {
	if u.BaseURL == "" || u.Token == "" {
		return nil, errors.New("upstash not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.BaseURL+"/get/"+url.PathEscape(key), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+u.Token)
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upstash get: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstash get %s: %d %s", key, resp.StatusCode, body)
	}
	var env struct {
		Result *string `json:"result"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("upstash response: %w", err)
	}
	if env.Result == nil {
		return nil, nil
	}
	var t Tunnel
	if err := json.Unmarshal([]byte(*env.Result), &t); err != nil {
		return nil, fmt.Errorf("tunnel row: %w", err)
	}
	return &t, nil
}
