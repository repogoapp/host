package tunnelgateway

import (
	"crypto/ed25519"
	"net/http"
	"strings"
	"testing"

	"github.com/repogoapp/host/tunnelwire"
)

// pluginHost is a host that answers plugin requests, and fails the test if a
// request to its environment ever arrives as a port tunnel's Request.
func (r *rig) pluginHost(answer func(*tunnelwire.PluginRequest) []tunnelwire.Message) *testHost {
	r.t.Helper()
	_, priv, _ := ed25519.GenerateKey(nil)
	h, err := r.connect(priv, gatewayID)
	if err != nil {
		r.t.Fatal(err)
	}
	go func() {
		for {
			m, err := h.read()
			if err != nil {
				return
			}
			switch m := m.(type) {
			case *tunnelwire.PluginRequest:
				for _, out := range answer(m) {
					_ = h.send(out)
				}
			case *tunnelwire.Request:
				r.t.Errorf("an environment request crossed as a port request: %s", m.Path)
			}
		}
	}()
	r.waitHost(h.id)
	return h
}

func envName(id string) string { return tunnelwire.EnvLabel(id) + "." + domain }

func (r *rig) doPath(method, host, path string, body *strings.Reader) *http.Response {
	r.t.Helper()
	req, err := http.NewRequest(method, r.url+path, nil)
	if body != nil {
		req, err = http.NewRequest(method, r.url+path, body)
	}
	if err != nil {
		r.t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("X-Hub-Signature-256", "sha256=abc")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestServesTheEnvironmentsPlugins(t *testing.T) {
	r := newRig(t)
	h := r.pluginHost(func(req *tunnelwire.PluginRequest) []tunnelwire.Message {
		return []tunnelwire.Message{&tunnelwire.Response{SessionID: req.SessionID, Status: 202, Body: req.Body, HeaderLines: []string{
			"X-Host: " + hostLineOf(req.HeaderLines),
			"X-Path: " + req.Path,
			"X-Method: " + req.Method,
			"X-Signature: " + headerOf(req.HeaderLines, "X-Hub-Signature-256"),
			"Set-Cookie: sid=1; Domain=" + domain + "; Path=/",
		}}}
	})

	resp := r.doPath(http.MethodPost, envName(h.id), "/deploys/notify/extra?x=1", strings.NewReader(`{"status":"failed"}`))
	if body := read(t, resp); resp.StatusCode != 202 || body != `{"status":"failed"}` {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	for header, want := range map[string]string{
		"X-Host":        envName(h.id),
		"X-Path":        "/deploys/notify/extra?x=1",
		"X-Method":      http.MethodPost,
		"X-Signature":   "sha256=abc",
		"Cache-Control": "no-store",
	} {
		if got := resp.Header.Get(header); got != want {
			t.Errorf("%s: %q, want %q", header, got, want)
		}
	}
	if c := resp.Header.Get("Set-Cookie"); strings.Contains(c, "Domain") {
		t.Errorf("cookie %q", c)
	}
}

func TestEnvironmentRefusals(t *testing.T) {
	r := newRig(t)
	h := r.pluginHost(func(req *tunnelwire.PluginRequest) []tunnelwire.Message {
		t.Errorf("host reached with %s", req.Path)
		return nil
	})
	other := strings.Repeat("0", 32)
	for name, c := range map[string]struct {
		host, path string
		want       int
	}{
		"not a host id":   {"h-nothex." + domain, "/a/b", http.StatusNotFound},
		"short host id":   {"h-" + h.id[:31] + "." + domain, "/a/b", http.StatusNotFound},
		"no method":       {envName(h.id), "/deploys", http.StatusNotFound},
		"root":            {envName(h.id), "/", http.StatusNotFound},
		"bad plugin name": {envName(h.id), "/../etc/passwd", http.StatusNotFound},
		"host not online": {envName(other), "/deploys/notify", http.StatusNotFound},
	} {
		if resp := r.doPath(http.MethodGet, c.host, c.path, nil); resp.StatusCode != c.want {
			t.Errorf("%s: %d, want %d", name, resp.StatusCode, c.want)
		}
	}
	resp := r.doPath(http.MethodPost, envName(h.id), "/deploys/notify", strings.NewReader(strings.Repeat("x", maxPluginBody+1)))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("body cap: %d", resp.StatusCode)
	}
}

func TestEnvironmentRefusesWebSockets(t *testing.T) {
	r := newRig(t)
	h := r.pluginHost(func(req *tunnelwire.PluginRequest) []tunnelwire.Message {
		t.Errorf("host reached with %s", req.Path)
		return nil
	})
	req, _ := http.NewRequest(http.MethodGet, r.url+"/deploys/notify", nil)
	req.Host = envName(h.id)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("%d", resp.StatusCode)
	}
}

func TestEnvironmentStreamsLargeAnswers(t *testing.T) {
	r := newRig(t)
	chunk := strings.Repeat("i", 512<<10)
	h := r.pluginHost(func(req *tunnelwire.PluginRequest) []tunnelwire.Message {
		out := []tunnelwire.Message{&tunnelwire.Response{SessionID: req.SessionID, Status: 200, Streaming: true, HeaderLines: []string{"Content-Type: application/octet-stream"}}}
		for i := range 4 {
			out = append(out, &tunnelwire.BodyChunk{SessionID: req.SessionID, Data: []byte(chunk), EndOfStream: i == 3})
		}
		return out
	})
	resp := r.doPath(http.MethodGet, envName(h.id), "/builds/install/job/app.ipa?token=t", nil)
	if body := read(t, resp); len(body) != 4*len(chunk) {
		t.Fatalf("got %d bytes", len(body))
	}
}

func headerOf(lines []string, name string) string {
	for _, l := range lines {
		if k, v, ok := strings.Cut(l, ": "); ok && strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}
