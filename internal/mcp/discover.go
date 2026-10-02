package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Discovery derives everything from a pasted URL, so no provider needs a
// definition: 401 resource_metadata → RFC 9728 → RFC 8414 endpoints → RFC 7591
// client_id. A 401 without authorization-server metadata wants a static key.

// maxDocument bounds every metadata and token read; anything larger is not one.
const maxDocument = 256 << 10

// oauthClient is what sign-in and refresh need, kept on the host: a device
// must never be able to name where a user's authorization code is sent.
type oauthClient struct {
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint,omitempty"`
	Scopes                []string `json:"scopes,omitempty"`
	// RFC 8707: some servers reject authorize and token calls without it.
	Resource string `json:"resource,omitempty"`
	ClientID string `json:"client_id,omitempty"`
	// What the server echoed back as registered for ClientID; empty means it
	// echoed nothing, taken as "everything we asked for".
	RedirectURIs []string `json:"redirect_uris,omitempty"`
}

type probe struct {
	url    string
	auth   AuthKind
	oauth  *oauthClient
	manual bool   // OAuth, but no client id without the user's help
	refuse string // the server's own words for refusing registration
}

var resourceMetadata = regexp.MustCompile(`(?i)resource_metadata="([^"]+)"`)

func (s *Service) discover(ctx context.Context, target string, redirects []string) (probe, error) {
	status, header, speaks, err := s.initialize(ctx, target)
	if err != nil {
		return probe{}, ErrInvalid.Errorf("could not reach that URL")
	}
	if status != http.StatusUnauthorized {
		// A site root or a 404 page is not an open MCP server, and silently
		// "connecting" to one is worse than saying so.
		if !speaks {
			return probe{}, ErrInvalid.Errorf("that URL didn't answer as an MCP server; the endpoint is often /mcp or /sse")
		}
		return probe{url: target, auth: AuthNone}, nil
	}

	metaURL := ""
	if m := resourceMetadata.FindStringSubmatch(header); m != nil {
		metaURL = m[1]
	} else {
		metaURL = resolve(target, "/.well-known/oauth-protected-resource")
	}
	// Absent on servers that predate RFC 9728, which may still serve RFC 8414
	// at their origin: only a missing authorization server means "static key".
	resource := s.fetchJSON(ctx, metaURL)
	authServer := firstString(resource["authorization_servers"])
	if authServer == "" {
		authServer = resolve(target, "/")
	}
	meta := s.authServerMetadata(ctx, authServer)
	if meta == nil {
		return probe{url: target, auth: AuthAPIKey}, nil
	}

	client := &oauthClient{
		AuthorizationEndpoint: meta["authorization_endpoint"].(string),
		TokenEndpoint:         meta["token_endpoint"].(string),
		Scopes:                withOfflineAccess(stringsOf(resource["scopes_supported"]), stringsOf(meta["scopes_supported"])),
	}
	if reg, ok := meta["registration_endpoint"].(string); ok {
		client.RegistrationEndpoint = reg
	}
	if r, ok := resource["resource"].(string); ok {
		client.Resource = r
	}
	p := probe{url: target, auth: AuthOAuth, oauth: client}
	if client.RegistrationEndpoint == "" {
		p.manual = true
		return p, nil
	}
	reg := s.register(ctx, client.RegistrationEndpoint, redirects)
	client.ClientID, client.RedirectURIs = reg.clientID, reg.redirects
	if reg.refused != "" {
		p.manual, p.refuse = true, reg.refused
	}
	return p, nil
}

// initialize sends a real MCP initialize: it draws the challenge from a server
// that wants auth and proves one that does not actually speaks MCP.
func (s *Service) initialize(ctx context.Context, target string) (int, string, bool, error) {
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "repogo", "version": "2"},
		},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	res, err := s.http.Do(req)
	if err != nil {
		return 0, "", false, err
	}
	defer res.Body.Close()
	speaks := false
	if ct := res.Header.Get("Content-Type"); res.StatusCode < 300 && (strings.Contains(ct, "json") || strings.Contains(ct, "event-stream")) {
		// Stop at the first sign of an envelope: an SSE response may never end.
		buf := make([]byte, 4096)
		var seen []byte
		for len(seen) < maxDocument && !speaks {
			n, err := res.Body.Read(buf)
			seen = append(seen, buf[:n]...)
			speaks = bytes.Contains(seen, []byte(`"jsonrpc"`))
			if err != nil {
				break
			}
		}
	}
	return res.StatusCode, res.Header.Get("WWW-Authenticate"), speaks, nil
}

// authServerMetadata tries the path-suffixed RFC 8414 form, the plain one, and
// the OIDC path some servers reuse.
func (s *Service) authServerMetadata(ctx context.Context, issuer string) map[string]any {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return nil
	}
	suffix := strings.TrimSuffix(u.Path, "/")
	for _, path := range []string{
		"/.well-known/oauth-authorization-server" + suffix,
		"/.well-known/oauth-authorization-server",
		"/.well-known/openid-configuration",
	} {
		doc := s.fetchJSON(ctx, resolve(issuer, path))
		_, a := doc["authorization_endpoint"].(string)
		_, t := doc["token_endpoint"].(string)
		if a && t {
			return doc
		}
	}
	return nil
}

type registration struct {
	clientID  string
	redirects []string
	// A 4xx: the server will never register us on these terms, so the user
	// needs to bring a client id. Distinct from a transient failure, which is
	// worth retrying at sign-in.
	refused string
}

func (s *Service) register(ctx context.Context, endpoint string, redirects []string) registration {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "RepoGo",
		"redirect_uris":              redirects,
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return registration{}
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := s.http.Do(req)
	if err != nil {
		return registration{}
	}
	defer res.Body.Close()
	doc := readJSON(res.Body)
	switch {
	case res.StatusCode < 300:
		id, _ := doc["client_id"].(string)
		return registration{clientID: id, redirects: stringsOf(doc["redirect_uris"])}
	case res.StatusCode < 500:
		return registration{refused: oauthError(doc, "registration refused ("+http.StatusText(res.StatusCode)+")")}
	default:
		return registration{}
	}
}

func (s *Service) fetchJSON(ctx context.Context, u string) map[string]any {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/json")
	res, err := s.http.Do(req)
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return nil
	}
	return readJSON(res.Body)
}

func readJSON(r io.Reader) map[string]any {
	var doc map[string]any
	if json.NewDecoder(io.LimitReader(r, maxDocument)).Decode(&doc) != nil {
		return nil
	}
	return doc
}

func oauthError(doc map[string]any, fallback string) string {
	for _, key := range []string{"error_description", "error"} {
		if v, ok := doc[key].(string); ok && v != "" {
			return v
		}
	}
	return fallback
}

func resolve(base, path string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	return u.ResolveReference(&url.URL{Path: path}).String()
}

// withOfflineAccess asks for a refresh token whenever the authorization server
// offers one: resource documents list what the server needs, not what keeps the
// connection alive. An unknown scope may be rejected, so it is never added blindly.
func withOfflineAccess(scopes, server []string) []string {
	const offline = "offline_access"
	if len(scopes) > 0 && !slices.Contains(scopes, offline) && slices.Contains(server, offline) {
		return append(scopes, offline)
	}
	return scopes
}

func stringsOf(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func firstString(v any) string {
	if list := stringsOf(v); len(list) > 0 {
		return list[0]
	}
	return ""
}
