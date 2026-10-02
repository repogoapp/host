package mcp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

// NativeRedirect is v1's web page, which hands the code on to the app's
// `repogo://mcp-oauth` without keeping it: many providers refuse to redirect
// to a custom scheme.
const NativeRedirect = "https://repogo.app/mcp/callback/native"

// LoopbackRedirect is for upstreams whose registration accepts nothing else;
// the app catches it on a one-shot listener at this port and path.
const LoopbackRedirect = "http://127.0.0.1:1456/mcp-oauth"

// A probe waits this long for the user to finish connecting.
const probeTTL = 10 * time.Minute

// pending is a probe kept on the host: the device gets a handle and only what
// it needs to pick the next screen, never the endpoints.
type pending struct {
	probe
	label   string
	project string
	expires time.Time

	// Set by OAuthStart; the verifier never leaves the host.
	redirect string
	verifier string
	state    string
}

// Probe is the answer to "what does this URL need?".
type Probe struct {
	ProbeID           string   `json:"probe_id"`
	URL               string   `json:"url"`
	Auth              AuthKind `json:"auth"`
	NeedsManualClient bool     `json:"needs_manual_client"`
	RegistrationError string   `json:"registration_error,omitempty"`
}

// Probe discovers what url needs and keeps the result. label names the
// connection (a preset's name reads better than a hostname) and project is
// where the user is connecting from: the server starts switched on there.
func (s *Service) Probe(ctx context.Context, rawURL, label, project string) (Probe, error) {
	target, err := normalizeURL(rawURL)
	if err != nil {
		return Probe{}, err
	}
	p, err := s.discover(ctx, target, []string{NativeRedirect})
	if err != nil {
		return Probe{}, err
	}
	id := token()
	s.mu.Lock()
	for k, v := range s.pending {
		if time.Now().After(v.expires) {
			delete(s.pending, k)
		}
	}
	s.pending[id] = &pending{probe: p, label: strings.TrimSpace(label), project: project, expires: time.Now().Add(probeTTL)}
	s.mu.Unlock()
	return Probe{ProbeID: id, URL: p.url, Auth: p.auth, NeedsManualClient: p.manual, RegistrationError: p.refuse}, nil
}

// Connect finishes a none or API-key probe. An OAuth server is created by
// OAuthFinish, once a token exists, so a connection never appears without
// its credential.
func (s *Service) Connect(probeID, apiKey string) (Server, error) {
	p, err := s.take(probeID)
	if err != nil {
		return Server{}, err
	}
	apiKey = strings.TrimSpace(apiKey)
	switch {
	case p.auth == AuthOAuth:
		return Server{}, ErrInvalid.Errorf("this server connects through sign-in")
	case p.auth == AuthAPIKey && apiKey == "":
		return Server{}, ErrInvalid.Errorf("this server needs an API key")
	}
	r := s.newRecord(p)
	r.Secret = apiKey
	return s.put(r)
}

// OAuthStart builds the authorize URL. clientID is the user's own, for an
// upstream that would not register RepoGo; loopback picks LoopbackRedirect.
func (s *Service) OAuthStart(ctx context.Context, probeID, clientID string, loopback bool) (authorizeURL, state string, err error) {
	s.mu.Lock()
	p, err := s.peekLocked(probeID)
	s.mu.Unlock()
	if err != nil {
		return "", "", err
	}
	if p.auth != AuthOAuth {
		return "", "", ErrInvalid.Errorf("this server does not use sign-in")
	}
	redirect := NativeRedirect
	if loopback {
		redirect = LoopbackRedirect
	}
	client := *p.oauth
	clientID = strings.TrimSpace(clientID)
	// Some upstreams accept a registration yet keep only some of its redirect
	// URIs, and the mismatch then fails in the browser where we cannot see it:
	// register again for exactly this one.
	covered := len(client.RedirectURIs) == 0 || slices.Contains(client.RedirectURIs, redirect)
	switch {
	case clientID != "":
		client.ClientID, client.RedirectURIs = clientID, nil
	case (client.ClientID == "" || !covered) && client.RegistrationEndpoint != "":
		reg := s.register(ctx, client.RegistrationEndpoint, []string{redirect})
		if reg.clientID == "" {
			detail := reg.refused
			if detail == "" {
				detail = p.refuse
			}
			if detail != "" {
				return "", "", ErrInvalid.Errorf("%s — create a client in that service and paste its client ID", detail)
			}
			return "", "", ErrInvalid.Errorf("this server could not register RepoGo for sign-in; try again")
		}
		client.ClientID, client.RedirectURIs = reg.clientID, reg.redirects
	case client.ClientID == "":
		return "", "", ErrInvalid.Errorf("this server needs a client ID to sign in")
	}

	u, err := url.Parse(client.AuthorizationEndpoint)
	if err != nil {
		return "", "", ErrInvalid.Errorf("the server's sign-in address is not a URL")
	}
	verifier, state := token(), token()
	challenge := sha256.Sum256([]byte(verifier))
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", client.ClientID)
	q.Set("redirect_uri", redirect)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	q.Set("code_challenge_method", "S256")
	q.Set("state", state)
	if len(client.Scopes) > 0 {
		q.Set("scope", strings.Join(client.Scopes, " "))
	}
	if client.Resource != "" {
		q.Set("resource", client.Resource)
	}
	u.RawQuery = q.Encode()

	s.mu.Lock()
	defer s.mu.Unlock()
	live, err := s.peekLocked(probeID)
	if err != nil {
		return "", "", err
	}
	live.oauth = &client
	live.redirect, live.verifier, live.state = redirect, verifier, state
	return u.String(), state, nil
}

// OAuthFinish exchanges the code the browser brought back and only then
// creates the server, so backing out of sign-in leaves nothing behind.
func (s *Service) OAuthFinish(ctx context.Context, probeID, code, state string) (Server, error) {
	s.mu.Lock()
	p, err := s.peekLocked(probeID)
	s.mu.Unlock()
	if err != nil {
		return Server{}, err
	}
	if p.verifier == "" {
		return Server{}, ErrInvalid.Errorf("sign-in was not started")
	}
	if state != p.state || strings.TrimSpace(code) == "" {
		return Server{}, ErrInvalid.Errorf("sign-in did not return a code")
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {p.redirect},
		"client_id":     {p.oauth.ClientID},
		"code_verifier": {p.verifier},
	}
	tok, err := s.tokenCall(ctx, p.oauth, form)
	if err != nil {
		return Server{}, err
	}
	if _, err := s.take(probeID); err != nil {
		return Server{}, err
	}
	r := s.newRecord(p)
	r.OAuth = p.oauth
	r.Secret, r.Refresh, r.ExpiresAt = tok.access, tok.refresh, tok.expiresAt
	return s.put(r)
}

func (s *Service) newRecord(p *pending) record {
	label := p.label
	if label == "" {
		label = hostOf(p.url)
	}
	projects := []string{}
	if strings.TrimSpace(p.project) != "" {
		projects = append(projects, Root(p.project))
	}
	return record{Server: Server{
		ID: serverID(p.url), Label: label, URL: p.url, Auth: p.auth,
		Projects: projects, CreatedAt: time.Now().UnixMilli(),
	}}
}

type grant struct {
	access, refresh string
	expiresAt       int64
}

// tokenCall posts to the token endpoint. It never follows a redirect: a code
// or refresh token replayed to another origin is a leak.
func (s *Service) tokenCall(ctx context.Context, c *oauthClient, form url.Values) (grant, error) {
	if c.Resource != "" {
		form.Set("resource", c.Resource)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return grant{}, ErrInvalid.Errorf("the server's token address is not a URL")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	client := *s.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return grant{}, ErrInvalid.Errorf("could not reach the server to sign in")
	}
	defer res.Body.Close()
	doc := readJSON(res.Body)
	access, _ := doc["access_token"].(string)
	if res.StatusCode >= 300 || access == "" {
		return grant{}, ErrInvalid.Errorf("%s", oauthError(doc, "sign-in failed"))
	}
	g := grant{access: access}
	g.refresh, _ = doc["refresh_token"].(string)
	if secs, ok := doc["expires_in"].(float64); ok && secs > 0 {
		g.expiresAt = time.Now().Add(time.Duration(secs) * time.Second).UnixMilli()
	}
	return g, nil
}

var errProbeGone = ErrInvalid.Errorf("that probe expired; check the URL again")

func (s *Service) peekLocked(id string) (*pending, error) {
	p := s.pending[id]
	if p == nil || time.Now().After(p.expires) {
		return nil, errProbeGone
	}
	return p, nil
}

// take consumes a probe, so a handle cannot be replayed.
func (s *Service) take(id string) (*pending, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.peekLocked(id)
	delete(s.pending, id)
	return p, err
}

// token is 43 characters: PKCE's minimum for a verifier.
func token() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
