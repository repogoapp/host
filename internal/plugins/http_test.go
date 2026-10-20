package plugins

import (
	"errors"
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
    ping: method(z.object({ name: z.string().optional() }), (input, ctx) => ({ ok: true, name: input.name ?? null, caller: ctx.caller }), { kind: "query", http: { auth: "none" } }),
    echo: method(z.object({ message: z.string() }), (input) => ({ echo: input.message }), { http: { auth: "none" } }),
    keyed: method(z.object({}), () => "secret", { http: true }),
    closed: method(z.object({}), () => "closed"),
    asks: method(z.object({}), () => "asked", { http: { auth: "none" }, confirm: "Sure?" }),
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
	if status, _, _ := serve(http.MethodGet, "/hello/keyed", ""); status != http.StatusUnauthorized {
		t.Fatalf("keyed: %d", status)
	}
	for _, path := range []string{"/hello/closed", "/hello/asks", "/hello/tail", "/hello/missing", "/nope/ping"} {
		if _, _, err := serve(http.MethodGet, path, ""); !errors.Is(err, tunnel.ErrNoRoute) {
			t.Errorf("%s: %v, want ErrNoRoute", path, err)
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
