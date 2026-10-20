package tunnel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/repogoapp/host/tunnelwire"
)

// fakePlugins records what reached it and answers with serve.
type fakePlugins struct {
	mu     sync.Mutex
	routes []tunnelwire.PluginRoute
	serve  func(tunnelwire.PluginRoute, *http.Request) (*http.Response, error)
}

func (p *fakePlugins) HasRoutes() bool { return true }

func (p *fakePlugins) ServeHTTP(_ context.Context, route tunnelwire.PluginRoute, req *http.Request) (*http.Response, error) {
	p.mu.Lock()
	p.routes = append(p.routes, route)
	p.mu.Unlock()
	return p.serve(route, req)
}

func (p *fakePlugins) reached() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.routes)
}

func answer(status int, body string, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}
}

func (c *fakeConn) plugin(t *testing.T, id, host, method, path string, lines []string, body string) (int, http.Header, string) {
	t.Helper()
	if host != "" {
		lines = append(lines, "Host: "+host)
	}
	if err := c.send(&tunnelwire.PluginRequest{SessionID: id, Method: method, Path: path, HeaderLines: lines, Body: []byte(body)}); err != nil {
		t.Fatal(err)
	}
	return c.reply(t, id)
}

// A plugin route keeps the host on the gateway with no tunnel open, and the
// request reaches the plugin as the caller sent it.
func TestServesPluginRoutes(t *testing.T) {
	g := newFakeGateway(t, "127.0.0.1")
	s, id := newService(t, g.addr, peers{})
	var got *http.Request
	var gotBody string
	plugins := &fakePlugins{serve: func(_ tunnelwire.PluginRoute, req *http.Request) (*http.Response, error) {
		got = req
		b, _ := io.ReadAll(req.Body)
		gotBody = string(b)
		return answer(http.StatusAccepted, "queued", http.Header{"X-Plugin": {"deploys"}}), nil
	}}
	s.cfg.Plugins = plugins
	run(t, s)
	c := g.accept(t)

	host := "h-" + string(id.ID) + "." + Domain
	status, header, body := c.plugin(t, "p1", host, "POST", "/deploys/notify/extra?x=1", []string{"X-Hub-Signature-256: sha256=abc"}, `{"status":"failed"}`)
	if status != http.StatusAccepted || body != "queued" || header.Get("X-Plugin") != "deploys" {
		t.Fatalf("%d %q %v", status, body, header)
	}
	if want := (tunnelwire.PluginRoute{Plugin: "deploys", Method: "notify", Rest: "extra"}); plugins.routes[0] != want {
		t.Fatalf("route %+v", plugins.routes[0])
	}
	if got.Method != "POST" || got.URL.Query().Get("x") != "1" || gotBody != `{"status":"failed"}` {
		t.Fatalf("request %s %s %q", got.Method, got.URL, gotBody)
	}
	if got.Header.Get("X-Hub-Signature-256") != "sha256=abc" || got.Header.Get("X-Forwarded-Host") != host {
		t.Fatalf("headers %v", got.Header)
	}
}

func TestPluginRefusals(t *testing.T) {
	g := newFakeGateway(t, "127.0.0.1")
	s, id := newService(t, g.addr, peers{})
	plugins := &fakePlugins{serve: func(route tunnelwire.PluginRoute, _ *http.Request) (*http.Response, error) {
		switch route.Method {
		case "hidden":
			return nil, ErrNoRoute
		case "broken":
			return nil, errors.New("worker crashed")
		}
		return answer(http.StatusOK, "served", nil), nil
	}}
	s.cfg.Plugins = plugins
	run(t, s)
	c := g.accept(t)
	own := "h-" + string(id.ID) + "." + Domain

	for name, req := range map[string][2]string{
		"another environment": {"h-" + strings.Repeat("0", 32) + "." + Domain, "/deploys/notify"},
		"a tunnel slug":       {"abcdefghij12." + Domain, "/deploys/notify"},
		"no Host line":        {"", "/deploys/notify"},
		"no method":           {own, "/deploys"},
		"a bad plugin name":   {own, "/../notify"},
	} {
		if status, _, body := c.plugin(t, name, req[0], "GET", req[1], nil, ""); status != http.StatusNotFound || body == "served" {
			t.Errorf("%s: %d %q", name, status, body)
		}
	}
	if n := plugins.reached(); n != 0 {
		t.Fatalf("refused requests reached the plugins %d times", n)
	}
	// A missing route and a closed one look the same.
	if status, _, _ := c.plugin(t, "hidden", own, "GET", "/deploys/hidden", nil, ""); status != http.StatusNotFound {
		t.Errorf("hidden: %d", status)
	}
	if status, _, _ := c.plugin(t, "broken", own, "GET", "/deploys/broken", nil, ""); status != http.StatusBadGateway {
		t.Errorf("broken: %d", status)
	}
	// The environment's name is never a port tunnel's.
	if status, _, _ := c.get(t, "port", strconv.Itoa(3000), own); status != http.StatusForbidden {
		t.Errorf("port request to the environment name: %d", status)
	}
}

func TestStreamsLargePluginAnswers(t *testing.T) {
	g := newFakeGateway(t, "127.0.0.1")
	s, id := newService(t, g.addr, peers{})
	ipa := strings.Repeat("i", 3*bufferedBodyLimit)
	s.cfg.Plugins = &fakePlugins{serve: func(tunnelwire.PluginRoute, *http.Request) (*http.Response, error) {
		return answer(http.StatusOK, ipa, http.Header{"Content-Type": {"application/octet-stream"}}), nil
	}}
	run(t, s)
	c := g.accept(t)
	_, _, body := c.plugin(t, "ipa", "h-"+string(id.ID)+"."+Domain, "GET", "/builds/install/job/app.ipa?token=t", nil, "")
	if body != ipa {
		t.Fatalf("got %d bytes, want %d", len(body), len(ipa))
	}
}
