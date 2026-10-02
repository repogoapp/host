package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func open(t *testing.T) *Service {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "mcp.json"), func(Changed) {})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// openServer answers initialize like an MCP server that needs no sign-in.
func openServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNoAuthServerConnectsAndIsOnWhereItWasAdded(t *testing.T) {
	s := open(t)
	var pushed []Changed
	s.onChange = func(c Changed) { pushed = append(pushed, c) }
	up := openServer(t)
	project := t.TempDir()

	p, err := s.Probe(context.Background(), up.URL+"/mcp", "Docs", project)
	if err != nil {
		t.Fatal(err)
	}
	if p.Auth != AuthNone {
		t.Fatalf("auth = %s, want none", p.Auth)
	}
	srv, err := s.Connect(p.ProbeID, "")
	if err != nil {
		t.Fatal(err)
	}
	if srv.Label != "Docs" || len(srv.Projects) != 1 || srv.Projects[0] != project {
		t.Fatalf("server = %+v", srv)
	}
	if len(pushed) != 1 || len(pushed[0].Servers) != 1 {
		t.Fatalf("pushed %+v, want the one-server list", pushed)
	}
	if _, err := s.Connect(p.ProbeID, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a probe handle must be single use, got %v", err)
	}

	got := s.ForTurn(context.Background(), filepath.Join(project, "sub"))
	if len(got) != 1 || got[0].Name != "docs" || got[0].URL != up.URL+"/mcp" || len(got[0].Headers) != 0 {
		t.Fatalf("ForTurn = %+v", got)
	}
	if other := s.ForTurn(context.Background(), t.TempDir()); len(other) != 0 {
		t.Fatalf("a project it was not switched on for got %+v", other)
	}
}

func TestAPIKeyServerSendsItsKeyAndNeverShowsIt(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // no resource metadata anywhere
	}))
	defer up.Close()
	s := open(t)
	project := t.TempDir()

	p, err := s.Probe(context.Background(), up.URL, "", project)
	if err != nil || p.Auth != AuthAPIKey {
		t.Fatalf("probe = %+v, %v; want api_key", p, err)
	}
	if _, err := s.Connect(p.ProbeID, " "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an empty key must be refused, got %v", err)
	}
	p, _ = s.Probe(context.Background(), up.URL, "", project)
	if _, err := s.Connect(p.ProbeID, "sk-secret"); err != nil {
		t.Fatal(err)
	}

	got := s.ForTurn(context.Background(), project)
	if len(got) != 1 || got[0].Headers[0].Name != "Authorization" || got[0].Headers[0].Value != "Bearer sk-secret" {
		t.Fatalf("ForTurn = %+v", got)
	}
	list, _ := json.Marshal(Changed{Servers: servers(s)})
	if strings.Contains(string(list), "sk-secret") {
		t.Fatalf("the public list carries the key: %s", list)
	}
	info, err := os.Stat(s.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("store mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	reopened, err := Open(s.path, func(Changed) {})
	if err != nil || len(reopened.ForTurn(context.Background(), project)) != 1 {
		t.Fatalf("reopen lost the server: %v", err)
	}
}

// fakeAuth is an MCP server behind OAuth: RFC 9728 metadata, RFC 8414
// endpoints, open registration, and a PKCE-checking token endpoint.
type fakeAuth struct {
	*httptest.Server
	challenge string
	issued    atomic.Int32
	refreshed atomic.Int32
	expiresIn int
}

func newFakeAuth(t *testing.T) *fakeAuth {
	f := &fakeAuth{expiresIn: 3600}
	mux := http.NewServeMux()
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+f.URL+`/.well-known/oauth-protected-resource"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"resource": f.URL + "/mcp", "authorization_servers": []string{f.URL}, "scopes_supported": []string{"read"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"authorization_endpoint": f.URL + "/authorize", "token_endpoint": f.URL + "/token",
			"registration_endpoint": f.URL + "/register", "scopes_supported": []string{"read", "offline_access"},
		})
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"client_id": "client-" + body.RedirectURIs[0], "redirect_uris": body.RedirectURIs})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
			if r.Form.Get("code") != "the-code" || base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge ||
				r.Form.Get("resource") != f.URL+"/mcp" {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"invalid_grant","error_description":"bad code or verifier"}`)
				return
			}
			f.issued.Add(1)
			fmt.Fprintf(w, `{"access_token":"access-1","refresh_token":"refresh-1","expires_in":%d}`, f.expiresIn)
		case "refresh_token":
			f.refreshed.Add(1)
			fmt.Fprint(w, `{"access_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`)
		}
	})
	return f
}

func TestOAuthServerSignsInWithPKCEAndRefreshes(t *testing.T) {
	up := newFakeAuth(t)
	up.expiresIn = 60 // inside the refresh window
	s := open(t)
	project := t.TempDir()

	p, err := s.Probe(context.Background(), up.URL+"/mcp", "Linear", project)
	if err != nil || p.Auth != AuthOAuth || p.NeedsManualClient {
		t.Fatalf("probe = %+v, %v", p, err)
	}
	if _, err := s.Connect(p.ProbeID, ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an OAuth probe must not connect without sign-in, got %v", err)
	}
	p, _ = s.Probe(context.Background(), up.URL+"/mcp", "Linear", project)
	authorize, state, err := s.OAuthStart(context.Background(), p.ProbeID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, authorize)
	up.challenge = q.Get("code_challenge")
	if q.Get("client_id") != "client-"+NativeRedirect || q.Get("redirect_uri") != NativeRedirect ||
		q.Get("state") != state || q.Get("scope") != "read offline_access" || q.Get("resource") != up.URL+"/mcp" {
		t.Fatalf("authorize query = %v", q)
	}
	if len(servers(s)) != 0 {
		t.Fatal("nothing may exist before the exchange succeeds")
	}
	if _, err := s.OAuthFinish(context.Background(), p.ProbeID, "the-code", "forged"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a mismatched state must be refused, got %v", err)
	}
	srv, err := s.OAuthFinish(context.Background(), p.ProbeID, "the-code", state)
	if err != nil {
		t.Fatal(err)
	}
	if srv.Auth != AuthOAuth || up.issued.Load() != 1 {
		t.Fatalf("server = %+v issued=%d", srv, up.issued.Load())
	}

	got := s.ForTurn(context.Background(), project)
	if len(got) != 1 || got[0].Headers[0].Value != "Bearer access-2" || up.refreshed.Load() != 1 {
		t.Fatalf("ForTurn = %+v refreshed=%d; want the refreshed token", got, up.refreshed.Load())
	}
	// Stored with the rotated refresh token and a fresh expiry: no second refresh.
	s.ForTurn(context.Background(), project)
	if up.refreshed.Load() != 1 {
		t.Fatalf("refreshed %d times, want once", up.refreshed.Load())
	}
}

func TestLoopbackSignInRegistersItsOwnRedirect(t *testing.T) {
	up := newFakeAuth(t)
	s := open(t)
	p, err := s.Probe(context.Background(), up.URL+"/mcp", "", "")
	if err != nil {
		t.Fatal(err)
	}
	authorize, _, err := s.OAuthStart(context.Background(), p.ProbeID, "", true)
	if err != nil {
		t.Fatal(err)
	}
	q := mustQuery(t, authorize)
	if q.Get("redirect_uri") != LoopbackRedirect || q.Get("client_id") != "client-"+LoopbackRedirect {
		t.Fatalf("authorize query = %v", q)
	}
}

func TestAWebPageIsNotAnMCPServer(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html>hello</html>")
	}))
	defer page.Close()
	if _, err := open(t).Probe(context.Background(), page.URL, "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want invalid", err)
	}
	if _, err := open(t).Probe(context.Background(), "ftp://example.com", "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want invalid", err)
	}
}

func TestSwitchesRenameAndRemove(t *testing.T) {
	s := open(t)
	up := openServer(t)
	p, _ := s.Probe(context.Background(), up.URL, "", "")
	srv, err := s.Connect(p.ProbeID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(srv.Projects) != 0 {
		t.Fatalf("connected from nowhere, yet on in %v", srv.Projects)
	}
	project := t.TempDir()
	if err := s.SetEnabled(srv.ID, project, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetEnabled(srv.ID, project, true); err != nil || len(servers(s)[0].Projects) != 1 {
		t.Fatalf("switching on twice must not duplicate: %v %v", servers(s)[0].Projects, err)
	}
	if err := s.Rename(srv.ID, "  Mine "); err != nil || servers(s)[0].Label != "Mine" {
		t.Fatalf("rename: %v %+v", err, servers(s))
	}
	if err := s.SetEnabled(srv.ID, project, false); err != nil || len(s.ForTurn(context.Background(), project)) != 0 {
		t.Fatalf("switched off, still applied: %v", err)
	}
	if err := s.Remove(srv.ID); err != nil || len(servers(s)) != 0 {
		t.Fatalf("remove: %v", err)
	}
	if err := s.Remove(srv.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want not found", err)
	}
}

// A change the file did not take must not stay in memory, where turns would
// use it until a restart silently undid it.
func TestFailedSaveRollsBack(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "mcp.json"), func(Changed) {})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Probe(context.Background(), openServer(t).URL, "Docs", "")
	if err != nil {
		t.Fatal(err)
	}
	srv, err := s.Connect(p.ProbeID, "")
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	if err := s.SetEnabled(srv.ID, project, true); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })

	if err := s.SetEnabled(srv.ID, project, false); err == nil {
		t.Fatal("switched off without saving")
	}
	if err := s.Rename(srv.ID, "Other"); err == nil {
		t.Fatal("renamed without saving")
	}
	if err := s.Remove(srv.ID); err == nil {
		t.Fatal("removed without saving")
	}
	list := servers(s)
	if len(list) != 1 || list[0].Label != "Docs" || len(list[0].Projects) != 1 {
		t.Fatalf("unsaved changes stuck: %+v", list)
	}
}

func TestRootIsTheMainCheckoutOfAWorktree(t *testing.T) {
	main := t.TempDir()
	if err := os.MkdirAll(filepath.Join(main, ".git", "worktrees", "feat"), 0o755); err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(main, ".git", "worktrees", "feat")+"\n"), 0o644)
	os.MkdirAll(filepath.Join(main, "pkg"), 0o755)

	for dir, want := range map[string]string{main: main, filepath.Join(main, "pkg"): main, wt: main} {
		if got := Root(dir); got != want {
			t.Errorf("Root(%s) = %s, want %s", dir, got, want)
		}
	}
}

func TestSlugsAreUniqueToolNamespaces(t *testing.T) {
	taken := map[string]bool{}
	for _, tc := range []struct{ label, want string }{
		{"Cloudflare Docs", "cloudflare_docs"}, {"mcp.stripe.com", "mcp_stripe_com"},
		{"Cloudflare  Docs!", "cloudflare_docs_2"}, {"!!!", "server"},
	} {
		got := slug(tc.label, taken)
		taken[got] = true
		if got != tc.want {
			t.Errorf("slug(%q) = %q, want %q", tc.label, got, tc.want)
		}
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func servers(s *Service) []Server {
	return s.List("").Servers
}

// RepoGo's own server is listed off by default, switched per project like a
// connected one, kept across a restart, and never renamed or removed.
func TestBuiltinSwitchesPerProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	var told []Changed
	s, err := Open(path, func(c Changed) { told = append(told, c) })
	if err != nil {
		t.Fatal(err)
	}
	project, other := t.TempDir(), t.TempDir()
	if got := s.List(project).Builtins; len(got) != 1 || got[0].ID != BuiltinID || len(got[0].Projects) != 0 {
		t.Fatalf("builtins %+v", got)
	}
	if s.BuiltinOn(BuiltinID, project) {
		t.Fatal("on before it was switched on")
	}
	if err := s.SetEnabled(BuiltinID, project, true); err != nil {
		t.Fatal(err)
	}
	if !s.BuiltinOn(BuiltinID, filepath.Join(project, "sub")) || s.BuiltinOn(BuiltinID, other) {
		t.Fatal("the switch did not land on its project only")
	}
	if len(told) != 1 || len(told[0].Builtins) != 1 || len(told[0].Builtins[0].Projects) != 1 {
		t.Fatalf("mcp.changed %+v", told)
	}
	if err := s.Rename(BuiltinID, "Mine"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("rename: %v", err)
	}
	if err := s.Remove(BuiltinID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("remove: %v", err)
	}

	reopened, err := Open(path, func(Changed) {})
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.BuiltinOn(BuiltinID, project) {
		t.Fatal("the switch did not survive a restart")
	}
	if err := reopened.SetEnabled(BuiltinID, project, false); err != nil || reopened.BuiltinOn(BuiltinID, project) {
		t.Fatalf("switch off: %v", err)
	}
}

// A connected server labelled RepoGo cannot take the built-in's name in a turn.
func TestAConnectedServerDoesNotTakeTheBuiltinsName(t *testing.T) {
	s := open(t)
	project := t.TempDir()
	s.servers = []record{{Server: Server{ID: "x", Label: "RepoGo", URL: "https://example.com/mcp", Auth: AuthNone, Projects: []string{Root(project)}}}}
	got := s.ForTurn(context.Background(), project)
	if len(got) != 1 || got[0].Name == BuiltinID {
		t.Fatalf("servers %+v", got)
	}
}

func TestEmptyListingUsesArrays(t *testing.T) {
	list := open(t).List("")
	if list.Servers == nil || list.Builtins == nil || list.Installed == nil {
		t.Fatalf("required collections must be arrays: %+v", list)
	}
	for _, builtin := range list.Builtins {
		if builtin.Projects == nil {
			t.Fatalf("required builtin arrays: %+v", builtin)
		}
	}
}
