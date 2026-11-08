package plugins

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/tunnel"
	"github.com/repogoapp/host/tunnelwire"
)

const httpPlugin = `import { definePlugin, method, stream, z } from "@repogo/plugins";
export default definePlugin({
  methods: {
    ping: method(z.object({ name: z.string().optional() }), (input, ctx) => ({ ok: true, name: input.name ?? null, caller: ctx.caller }), { effect: "read", http: { public: "none" } }),
    echo: method(z.object({ message: z.string() }), (input) => ({ echo: input.message }), { http: { public: "none" } }),
    keyed: method(z.object({}), () => "secret", { http: true }),
    fails: method(z.object({}), () => { throw new Error("fetch https://api.example.com/?token=sk_live_secret failed"); }, { effect: "read", http: { public: "none" } }),
    local: method(z.object({}), (input, ctx) => ({ caller: ctx.caller }), { effect: "read", http: { device: "none" } }),
    closed: method(z.object({}), () => "closed"),
    asks: method(z.object({}), () => "asked", { http: { public: "none", device: "none" }, approve: "Sure?" }),
    tail: stream(z.object({}), async function* () { yield 1; }),
  },
});
`

func TestServesMethodsOpenToHTTP(t *testing.T) {
	dir := sdk(t)
	s, _ := newService(t, dir)
	s.cfg.Global = filepath.Join(project(t, dir, map[string]string{"hello": httpPlugin}), ".repogo", "plugins")
	if !s.HasRoutes() {
		t.Fatal("HasRoutes is false with ping open")
	}
	serve := func(method, path, body string) (int, string, error) {
		route, _ := tunnelwire.ParsePluginPath(strings.Split(path, "?")[0])
		req := httptest.NewRequest(method, "https://h.example"+path, strings.NewReader(body))
		resp, err := s.ServeHTTP(t.Context(), route, req)
		if err != nil {
			return 0, "", err
		}
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b), nil
	}

	if status, body, err := serve(http.MethodGet, "/hello/ping?name=jo", ""); err != nil || status != 200 || body != `{"ok":true,"name":"jo","caller":"http"}` {
		t.Fatalf("ping: %d %s %v", status, body, err)
	}
	if status, body, _ := serve(http.MethodPost, "/hello/echo", `{"message":"hi"}`); status != 200 || body != `{"echo":"hi"}` {
		t.Fatalf("echo: %d %s", status, body)
	}
	if status, _, _ := serve(http.MethodPost, "/hello/echo", `{}`); status != http.StatusBadRequest {
		t.Fatalf("echo without a message: %d", status)
	}
	if status, _, _ := serve(http.MethodPost, "/hello/echo", `not json`); status != http.StatusBadRequest {
		t.Fatalf("echo with a bad body: %d", status)
	}
	if status, _, _ := serve(http.MethodGet, "/hello/echo?message=hi", ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("a write by GET: %d", status)
	}
	if status, body, _ := serve(http.MethodGet, "/hello/fails", ""); status != http.StatusInternalServerError || body != `{"error":"Internal server error"}` {
		t.Fatalf("an unexpected error: %d %s", status, body)
	}
	for _, path := range []string{"/hello/keyed", "/hello/local", "/hello/closed", "/hello/asks", "/hello/tail", "/hello/missing", "/nope/ping"} {
		if _, _, err := serve(http.MethodGet, path, ""); !errors.Is(err, tunnel.ErrNoRoute) {
			t.Errorf("%s: %v, want ErrNoRoute", path, err)
		}
	}
}

func TestServesTheDeviceAddress(t *testing.T) {
	dir := sdk(t)
	s, _ := newService(t, dir)
	s.cfg.Global = filepath.Join(project(t, dir, map[string]string{"hello": httpPlugin}), ".repogo", "plugins")
	srv := httptest.NewServer(http.HandlerFunc(s.ServeDevice))
	t.Cleanup(srv.Close)
	get := func(path string, header ...string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		if len(header) == 2 {
			req.Header.Set(header[0], header[1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if status, body := get("/hello/local"); status != 200 || body != `{"caller":"device"}` {
		t.Fatalf("local: %d %s", status, body)
	}
	if status, body := get("/hello"); status != 200 || !strings.Contains(body, `"name":"local"`) || strings.Contains(body, `"name":"ping"`) {
		t.Fatalf("listing: %d %s", status, body)
	}
	if status, _ := get("/hello/local", "Sec-Fetch-Site", "cross-site"); status != http.StatusForbidden {
		t.Fatalf("a browser's call: %d", status)
	}
	// Public-only, a key's, an approve's and a stream's methods aren't served here.
	for _, path := range []string{"/hello/ping", "/hello/keyed", "/hello/closed", "/hello/asks", "/hello/tail", "/nope/local", "/nope"} {
		if status, _ := get(path); status != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", path, status)
		}
	}
}

func TestNoRoutesWithoutAnOpenMethod(t *testing.T) {
	dir := sdk(t)
	s, _ := newService(t, dir)
	s.cfg.Global = filepath.Join(project(t, dir, map[string]string{"notes": notesPlugin}), ".repogo", "plugins")
	if s.HasRoutes() {
		t.Fatal("HasRoutes is true with no method open to HTTP")
	}
}

func TestKeyOpensOnlyItsPlugin(t *testing.T) {
	dir := sdk(t)
	s, _ := newService(t, dir)
	s.cfg.Global = filepath.Join(project(t, dir, map[string]string{"hello": httpPlugin, "notes": notesPlugin}), ".repogo", "plugins")
	keyed := func(key string) error {
		route, _ := tunnelwire.ParsePluginPath("/hello/keyed")
		req := httptest.NewRequest(http.MethodPost, "https://h.example/hello/keyed", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := s.ServeHTTP(t.Context(), route, req)
		if err == nil && resp.StatusCode != http.StatusOK {
			t.Fatalf("keyed with a key: %d", resp.StatusCode)
		}
		return err
	}

	list, err := s.Keys(t.Context())
	if err != nil || len(list) != 1 || list[0].Plugin != "hello" || list[0].Key != "" || strings.Join(list[0].Methods, ",") != "keyed" {
		t.Fatalf("Keys before one is made: %+v %v", list, err)
	}
	if !errors.Is(keyed(""), tunnel.ErrNoRoute) {
		t.Fatal("keyed answered with no key made")
	}
	made, err := s.CreateKey(t.Context(), "hello")
	if err != nil || !strings.HasPrefix(made.Key, "rgk_") || made.CreatedAt == 0 {
		t.Fatalf("CreateKey: %+v %v", made, err)
	}
	if err := keyed(made.Key); err != nil {
		t.Fatalf("keyed with its key: %v", err)
	}
	for name, key := range map[string]string{"no key": "", "a wrong key": made.Key + "x"} {
		if !errors.Is(keyed(key), tunnel.ErrNoRoute) {
			t.Errorf("%s: answered", name)
		}
	}
	if _, err := s.CreateKey(t.Context(), "notes"); !errors.Is(err, ErrNoKeyedPlugin) {
		t.Fatalf("a plugin with no keyed method got a key: %v", err)
	}
	again, _ := s.CreateKey(t.Context(), "hello")
	if !errors.Is(keyed(made.Key), tunnel.ErrNoRoute) || keyed(again.Key) != nil {
		t.Fatal("a replaced key still answers, or the new one doesn't")
	}
	if err := s.DeleteKey("hello"); err != nil || !errors.Is(keyed(again.Key), tunnel.ErrNoRoute) {
		t.Fatalf("a deleted key still answers: %v", err)
	}
}

// memKeys is plugin_keys in memory.
type memKeys map[string]string

func (m memKeys) PluginKey(plugin string) (string, int64, error) {
	if key := m[plugin]; key != "" {
		return key, 1, nil
	}
	return "", 0, nil
}
func (m memKeys) SetPluginKey(plugin, key string, _ int64) error { m[plugin] = key; return nil }
func (m memKeys) DeletePluginKey(plugin string) error            { delete(m, plugin); return nil }

func TestServedListsEachMethodsURLs(t *testing.T) {
	dir := sdk(t)
	s, _ := newService(t, dir)
	s.cfg.Global = filepath.Join(project(t, dir, map[string]string{"hello": httpPlugin, "notes": notesPlugin}), ".repogo", "plugins")
	served, err := s.Served(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range served {
		for _, u := range m.URLs {
			got[m.Plugin+"."+m.Method+" "+string(u.Address)] = u.URL + map[bool]string{true: " key"}[u.Key]
		}
	}
	public, device := "https://h-0123456789abcdef0123456789abcdef.repogo.dev/hello/", "http://127.0.0.1:47444/hello/"
	want := map[string]string{
		"hello.ping public": public + "ping", "hello.echo public": public + "echo", "hello.fails public": public + "fails",
		"hello.keyed public": public + "keyed key", "hello.local device": device + "local",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("Served:\n got  %v\n want %v", got, want)
	}
	if only, _ := s.Served(t.Context(), "notes"); len(only) != 0 {
		t.Fatalf("a plugin serving nothing: %+v", only)
	}
}
