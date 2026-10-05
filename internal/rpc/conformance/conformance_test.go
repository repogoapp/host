// Package conformance checks that a method answers the same however it was
// reached: in process, over the loopback WebSocket, or through the relay.
package conformance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/device"
	"github.com/repogo/host/internal/forward"
	"github.com/repogo/host/internal/handshake"
	"github.com/repogo/host/internal/hostclient"
	"github.com/repogo/host/internal/jsonrpc"
	"github.com/repogo/host/internal/rpc"
	"github.com/repogo/host/internal/rpc/registry"
	"github.com/repogo/host/internal/testhost"
)

// check is one method and a key its answer must carry: not byte equality,
// since `devices.list` rightly reports a different `you` to each caller.
type check struct {
	method string
	params any
	key    string // a key the result object must carry
}

// vocabulary is driven against a host whose files service serves root, which
// holds readme.txt. Mutations aim at a missing folder so every transport
// answers the same.
func vocabulary(root string) []check {
	file, missing := filepath.Join(root, "readme.txt"), filepath.Join(root, "missing")
	return []check{
		{method: "cloud.projects.list", params: map[string]any{"cwd": root}, key: "projects"},
		{method: "tools.logins", key: "waiting"},
		{method: "tools.login_cancel", params: map[string]any{"kind": "vercel"}, key: "ok"},
		{method: "chats.neighbors", params: map[string]any{"chat_id": "claude:missing"}, key: "previous"},
		{method: "chats.list", params: map[string]any{"limit": 5}, key: "chats"},
		{method: "chats.info", params: map[string]any{"chat_id": "claude:missing"}, key: "id"},
		{method: "chats.messages", params: map[string]any{"chat_id": "claude:missing", "tail": true}, key: "events"},
		{method: "chats.unsubscribe", key: "ok"},
		{method: "chats.queue", params: map[string]any{"chat_id": "claude:missing"}, key: "queued"},
		{method: "chats.tools_subscribe", params: map[string]any{"chat_id": "claude:missing", "call_ids": []string{"c1"}}, key: "tools"},
		{method: "chats.tools_unsubscribe", params: map[string]any{"chat_id": "claude:missing", "call_ids": []string{"c1"}}, key: "ok"},
		{method: "fs.stop", key: "ok"},
		{method: "host.status", key: "version"},
		{method: "devices.list", key: "peers"},
		{method: "turns.list", key: "turns"},
		{method: "terminal.list", key: "sessions"},
		{method: "fs.list", params: map[string]any{"path": root}, key: "entries"},
		{method: "fs.read", params: map[string]any{"path": file}, key: "content"},
		{method: "fs.write", params: map[string]any{"path": file, "content": []byte("hello")}, key: "path"},
		{method: "fs.replace", params: map[string]any{"path": file, "old": "hello", "new": "hello"}, key: "path"},
		{method: "fs.search", params: map[string]any{"path": root, "query": "hello", "mode": "content"}, key: "results"},
		{method: "fs.mkdir", params: map[string]any{"path": filepath.Join(missing, "new")}, key: "ok"},
		{method: "fs.rename", params: map[string]any{"path": filepath.Join(missing, "a"), "new_path": filepath.Join(missing, "b")}, key: "ok"},
		{method: "fs.delete", params: map[string]any{"path": filepath.Join(missing, "a")}, key: "ok"},
		{method: "fs.new_project", params: map[string]any{"name": ".refused"}, key: "path"},
		{method: "fs.add_project", params: map[string]any{"path": missing}, key: "path"},
		// A mirror pull with no rows and no cursor: the empty case is the one every
		// client starts from, and it must answer the same however it was reached.
		{method: "sync.pull", params: map[string]any{"family": "chats"}, key: "upsert"},
		{method: "project.list", key: "projects"},
		{method: "mcp.list", params: map[string]any{"project": root}, key: "installed"},
		{method: "mcp.rename", params: map[string]any{"id": "missing", "label": "x"}, key: "ok"},
		{method: "mcp.set_enabled", params: map[string]any{"id": "missing", "project": root, "enabled": true}, key: "ok"},
		{method: "mcp.remove", params: map[string]any{"id": "missing"}, key: "ok"},
		{method: "tunnels.list", key: "tunnels"},
		{method: "builds.apps", params: map[string]any{"project": root}, key: "apps"},
		{method: "builds.list", params: map[string]any{"project": root}, key: "builds"},
		{method: "builds.publish", params: map[string]any{"request_id": "00000000000000000000000000000001", "project": root, "path": "missing.xcodeproj", "target": "Demo", "version": "1.0.0", "build_number": "1"}, key: "publication"},
		{method: "builds.publish_status", params: map[string]any{"publication_id": "missing"}, key: "publication"},
		{method: "builds.publish_list", params: map[string]any{"project": root, "path": "Demo.xcodeproj", "target": "Demo"}, key: "publications"},
		{method: "builds.publish_log", params: map[string]any{"publication_id": "missing", "offset": 0}, key: "data"},
		{method: "builds.publish_cancel", params: map[string]any{"publication_id": "missing"}, key: "ok"},
		{method: "env.sources.list", key: "sources"},
		{method: "env.sources.set", params: map[string]any{"handle": "readme", "path": file}, key: "source"},
		{method: "env.sources.remove", params: map[string]any{"handle": "missing"}, key: "ok"},
		{method: "env.read", params: map[string]any{"handles": []string{"missing"}}, key: "results"},
		{method: "env.pending", key: "requests"},
		{method: "env.provide", params: map[string]any{"request_id": "missing", "approved": false}, key: "ok"},
		{method: "browser.pending", key: "requests"},
		{method: "browser.respond", params: map[string]any{"request_id": "missing", "result": map[string]any{"ok": true}}, key: "ok"},
	}
}

// localOnly must be refused to every caller that did not prove it is on this
// machine. Listed here so the refusal is asserted rather than assumed.
var localOnly = []string{"pair.begin", "pair.status", "pair.reusable", "pair.reusable_revoke", "hosts.release"}

// unchecked is the vocabulary this suite deliberately does not drive, each with
// a reason. Anything else new fails TestEveryMethodIsAccountedFor, which is the
// point: a family added without a case here does not go unnoticed.
var unchecked = map[string]string{
	"host.setup":        "shells out to gh and every agent CLI and asks the npm registry for versions; composition covered in internal/hostsetup",
	"host.update":       "downloads a release and restarts the host; covered in internal/hostupdate",
	"host.set_stops_at": "needs a cloud session the test host does not have; covered in internal/hostinfo",

	"devices.register_push": "paired-device token storage; loopback callers are denied",

	"pair.complete":        "admits a device; driven end to end by bench/e2e instead",
	"limits.read":          "reads the Keychain, an undocumented endpoint, and a codex subprocess",
	"limits.reset":         "spends a Codex reset credit; outcome parsing is in agentusage",
	"tools.update":         "runs the user's package manager",
	"tools.install":        "same, or the tool's installer",
	"tools.login_start":    "runs a CLI's sign-in against its provider; covered with a fake gh in internal/github and in internal/clilogin",
	"tools.login_complete": "same",
	"tools.logout":         "runs a CLI's sign-out on this machine; covered with a fake CLI in internal/clitool",
	"tools.uninstall":      "removes a CLI from this machine; covered with a fake CLI in internal/clitool",
	"vercel.logs":          "runs `vercel logs` against Vercel; covered with a fake CLI in internal/vercel",
	"vercel.deployments":   "asks Vercel's API through the CLI; covered with a fake CLI in internal/vercel",
	"cloud.env.list":       "runs the provider's CLI; covered with fake CLIs in internal/cloudenv and each provider",
	"cloud.env.value":      "runs the provider's CLI; covered with fake CLIs in internal/cloudenv and internal/vercel",
	"cloud.env.set":        "writes through the provider's CLI; covered with fake CLIs in internal/cloudenv and each provider",
	"cloud.env.remove":     "writes through the provider's CLI; covered with fake CLIs in internal/cloudenv and each provider",
	"turns.get":            "needs a running turn",
	"turns.stop":           "needs a running turn",
	"turns.edit_queued":    "needs a queued turn; covered in internal/agent",
	"turns.remove_queued":  "needs a queued turn; covered in internal/agent",
	"turns.send_queued":    "needs a queued turn; covered in internal/agent",
	"project.rename":       "needs a listed project; covered in internal/store",
	"project.pin":          "needs a listed project; covered in internal/store",
	"turns.respond":        "needs a turn waiting on approval",
	"chats.resolve":        "needs a chat cache; covered in the chats family suite",
	"chats.update":         "writes provider files; covered in the chats family suite",
	"chats.delete":         "removes provider files; covered in the chats family suite",
	"chats.stop":           "needs a running turn; covered in internal/agent",
	"chats.handoff":        "opens a terminal window on the Mac; the command is covered in internal/handoff",
	"chats.subagent":       "needs a session on disk; covered in the session package",
	"chats.send":           "would run an agent",
	"chats.subscribe":      "needs the live push lane",

	"fs.watch":          "starts a watcher and pushes; covered in the projectwatch package",
	"git.status":        "needs a real repository; covered in the git package",
	"git.changes":       "same",
	"git.branches":      "same",
	"git.pull":          "mutates repositories; covered with temporary repos in the git package",
	"git.reset_hard":    "same",
	"git.switch_branch": "same",
	"git.create_branch": "same",
	"git.commit_push":   "same",

	"github.repos": "shells out to gh, which a build machine need not have, and it needs a signed-in account",
	"github.clone": "same, and it downloads a repository",

	"github.pr_create": "shells out to gh against a real repository with a remote, and opens a pull request",
	"github.publish":   "same, and it creates a repository on GitHub; covered with a fake gh in internal/github",

	"usage.daily":   "scans the user's agent transcripts; covered with temporary homes in internal/agents (shipping_test.go)",
	"usage.history": "scans the user's agent transcripts; covered with temporary homes in internal/agents (shipping_test.go)",

	"forward.fetch":       "driven against a real dev server by TestForwardFetchOverEveryTransport",
	"forward.pipe_open":   "the pipe lane pushes, and pushes only reach a relay-side client; covered in internal/forward",
	"forward.pipe_send":   "same",
	"forward.pipe_close":  "same",
	"project.detect_icon": "needs a real project directory; covered by the project core",

	"chats.start": "spawns an agent; covered in internal/chat and the Manager",

	"ports.list": "shells out to list every listening socket on the machine; covered in internal/ports",
	"ports.kill": "signals real processes",

	"git.diff":  "needs a real repository; covered in internal/git",
	"git.patch": "same",

	"github.avatar": "fetches the signed-in account's picture from GitHub",

	"actions.list":  "needs a real project directory; covered in the actions package",
	"actions.run":   "runs a shell command from the project's actions.json",
	"actions.start": "same, detached",
	"actions.stop":  "needs a detached run",

	"builds.start":        "spawns xcodebuild or Gradle; covered with fakes in internal/builds",
	"builds.get":          "needs a build on disk; covered in internal/builds",
	"builds.cancel":       "needs a running build; covered in internal/builds",
	"builds.delete":       "needs a finished build; covered in internal/builds",
	"builds.log":          "needs a build on disk; covered in internal/builds",
	"builds.install_link": "needs a finished build and an open tunnel; covered in internal/builds",
	"builds.app_numbers":  "spawns xcodebuild; covered with a fake in internal/builds",

	"terminal.create":      "spawns a login shell; covered in the terminal package",
	"terminal.subscribe":   "pushes the tab set; covered in the terminal package",
	"terminal.unsubscribe": "same",
	"terminal.attach":      "needs a live session",
	"terminal.detach":      "same",
	"terminal.input":       "same",
	"terminal.resize":      "same",
	"terminal.close":       "same",

	"mcp.probe":        "reaches the pasted URL; covered against httptest servers in internal/mcp",
	"mcp.connect":      "needs a live probe; covered in internal/mcp",
	"mcp.oauth_start":  "same",
	"mcp.oauth_finish": "same, and it calls the server's token endpoint",

	"hosts.claim":   "paired devices only, so it differs by transport on purpose; driven by TestPairedOnlyMethods",
	"tunnels.open":  "same",
	"tunnels.close": "same",
}

// --- the host under test -----------------------------------------------------

type host struct {
	root     string
	router   *rpc.Router
	devices  *device.Store
	wsURL    string
	relayURL string
	self     device.ID
}

// newHost serves every family over temporary data on all three transports;
// extra registers methods only a test needs, before anything can call them.
func newHost(t *testing.T, extra ...func(*rpc.Router)) *host {
	t.Helper()
	th := testhost.New(t)
	devices, root := th.Devices, th.Root
	router, err := registry.New(th.Config)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	for _, register := range extra {
		register(router)
	}

	tr := testhost.Serve(t, devices, router)
	return &host{
		root:     root,
		router:   router,
		devices:  devices,
		wsURL:    tr.WS,
		relayURL: tr.Relay,
		self:     devices.Identity().ID,
	}
}

func (h *host) pair(t *testing.T) *device.Identity {
	t.Helper()
	id, err := device.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.devices.Add(device.Peer{
		ID: id.ID, Public: id.Public, Label: "Conformance phone", Platform: "ios",
	}); err != nil {
		t.Fatalf("pair: %v", err)
	}
	return id
}

// caller is one way of reaching the host. The suite runs the same table through
// each of them.
type caller struct {
	name string
	call func(t *testing.T, method string, params any) (json.RawMessage, error)
}

func dial(t *testing.T, cfg hostclient.Config) *hostclient.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := hostclient.Dial(ctx, cfg)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func dialWS(t *testing.T, h *host, phone *device.Identity) *hostclient.Client {
	return dial(t, hostclient.Config{
		URL: h.wsURL, Identity: phone, GroupID: h.devices.GroupID(),
		Role: handshake.RoleClient, Platform: "ios",
	})
}

func callers(t *testing.T, h *host) []caller {
	t.Helper()
	phone := h.pair(t)
	overWS := dialWS(t, h, phone)
	overRelay := dial(t, hostclient.Config{
		URL: h.relayURL, Identity: phone, GroupID: h.devices.GroupID(),
		Role: handshake.RoleClient, Platform: "ios", Host: h.self, HostPublic: h.devices.Identity().Public,
	})

	remote := func(c *hostclient.Client) func(*testing.T, string, any) (json.RawMessage, error) {
		return func(t *testing.T, method string, params any) (json.RawMessage, error) {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var out json.RawMessage
			err := c.Call(ctx, method, params, &out)
			return out, err
		}
	}

	return []caller{
		// In process: what the loopback HTTP surface does, minus the socket.
		{name: "in-process", call: func(t *testing.T, method string, params any) (json.RawMessage, error) {
			t.Helper()
			b, err := json.Marshal(params)
			if err != nil {
				t.Fatal(err)
			}
			return h.router.Call(context.Background(), rpc.Caller{Device: h.self, Scope: rpc.ScopeLocal}, method, b)
		}},
		{name: "loopback-ws", call: remote(overWS)},
		{name: "relay", call: remote(overRelay)},
	}
}

// --- the cases ---------------------------------------------------------------

// The whole point: one method, three ways in, same answer. Sameness, not
// success: a refusal counts as long as every transport refuses the same way.
func TestVocabularyAnswersTheSameOnEveryTransport(t *testing.T) {
	h := newHost(t)
	all := callers(t, h)

	for _, want := range vocabulary(h.root) {
		t.Run(want.method, func(t *testing.T) {
			type outcome struct {
				transport string
				code      int // 0 on success
			}
			outcomes := make([]outcome, 0, len(all))

			for _, c := range all {
				out, err := c.call(t, want.method, want.params)
				if err != nil {
					outcomes = append(outcomes, outcome{c.name, codeOf(err)})
					continue
				}
				var got map[string]json.RawMessage
				if err := json.Unmarshal(out, &got); err != nil {
					t.Fatalf("%s/%s: result is not an object: %v", c.name, want.method, err)
				}
				if _, ok := got[want.key]; !ok {
					t.Errorf("%s/%s: result has no %q (got %v)",
						c.name, want.method, want.key, slices.Collect(maps.Keys(got)))
				}
				outcomes = append(outcomes, outcome{c.name, 0})
			}

			first := outcomes[0]
			for _, o := range outcomes[1:] {
				if o.code != first.code {
					t.Errorf("%s answered %d over %s but %d over %s — the same method must not "+
						"depend on how the caller arrived",
						want.method, first.code, first.transport, o.code, o.transport)
				}
			}
		})
	}
}

// Both remote transports refuse a local-only method as denied, not missing:
// "your build is old" and "you are not allowed" send a user to different places.
func TestLocalOnlyMethodsAreRefusedRemotely(t *testing.T) {
	h := newHost(t)
	for _, c := range callers(t, h) {
		if c.name == "in-process" {
			continue // this caller IS the machine
		}
		for _, method := range localOnly {
			t.Run(c.name+"/"+method, func(t *testing.T) {
				_, err := c.call(t, method, nil)
				var rpcErr *jsonrpc.Error
				if !errors.As(err, &rpcErr) {
					t.Fatalf("%s answered %v, want a JSON-RPC error", method, err)
				}
				if rpcErr.Code != jsonrpc.CodeDenied {
					t.Errorf("%s answered code %d, want %d (denied)",
						method, rpcErr.Code, jsonrpc.CodeDenied)
				}
			})
		}
	}
}

// Claiming the host and opening a tunnel act for the phone that asks, so the
// machine itself is refused and both remote transports are served.
func TestPairedOnlyMethods(t *testing.T) {
	h := newHost(t)
	nonce := strings.Repeat("A", 43)
	expires := time.Now().Add(time.Hour).UnixMilli()
	for _, c := range callers(t, h) {
		t.Run(c.name, func(t *testing.T) {
			claim := map[string]any{"uid": "conformance_uid", "nonce": nonce}
			open := map[string]any{"slug": "conformance" + strings.ReplaceAll(c.name, "-", ""), "port": 3000, "expires_at": expires}
			if c.name == "in-process" {
				for method, params := range map[string]any{"hosts.claim": claim, "tunnels.open": open} {
					if _, err := c.call(t, method, params); !isCode(err, jsonrpc.CodeDenied) {
						t.Errorf("%s from the machine answered %v, want denied", method, err)
					}
				}
				return
			}
			for _, step := range []struct {
				method string
				params any
				key    string
			}{
				{"hosts.claim", claim, "signature"},
				{"tunnels.open", open, "tunnel"},
				{"tunnels.close", map[string]any{"slug": open["slug"]}, "ok"},
			} {
				out, err := c.call(t, step.method, step.params)
				if err != nil {
					t.Fatalf("%s: %v", step.method, err)
				}
				var got map[string]json.RawMessage
				if err := json.Unmarshal(out, &got); err != nil || got[step.key] == nil {
					t.Fatalf("%s: result %s has no %q", step.method, out, step.key)
				}
			}
		})
	}
}

// An unknown method answers method-not-found and leaves the connection usable:
// a client from a newer build must not be able to kill its own socket by asking
// for something this host has not learned.
func TestUnknownMethodIsAnErrorNotADisconnect(t *testing.T) {
	h := newHost(t)
	for _, c := range callers(t, h) {
		t.Run(c.name, func(t *testing.T) {
			_, err := c.call(t, "devices.teleport", nil)
			if !isCode(err, jsonrpc.CodeMethodNotFound) && !errors.Is(err, rpc.ErrUnsupported) {
				t.Fatalf("unknown method answered %v, want method-not-found", err)
			}
			if _, err := c.call(t, "devices.list", nil); err != nil {
				t.Fatalf("connection unusable afterwards: %v", err)
			}
		})
	}
}

// Every registered method is driven here or explicitly excused. Without this a
// family lands with no conformance coverage and nothing says so.
func TestEveryMethodIsAccountedFor(t *testing.T) {
	h := newHost(t)

	covered := map[string]bool{}
	for _, c := range vocabulary(h.root) {
		covered[c.method] = true
	}
	for _, m := range localOnly {
		covered[m] = true
	}

	for _, name := range h.router.Names() {
		if covered[name] {
			continue
		}
		if _, excused := unchecked[name]; excused {
			continue
		}
		t.Errorf("%s is registered but neither driven nor listed in `unchecked` with a reason", name)
	}
	for name := range unchecked {
		if !slices.Contains(h.router.Names(), name) {
			t.Errorf("`unchecked` excuses %s, which no longer exists", name)
		}
	}
}

// Forwarding's whole job is carrying bytes end to end, base64 in JSON and, over
// the relay, inside a binary envelope, so it runs against a real dev server.
func TestForwardFetchOverEveryTransport(t *testing.T) {
	h := newHost(t)

	const page = "<!doctype html><h1>it works</h1>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, "%s<!--%s-->", page, r.URL.Path)
	}))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port

	for _, c := range callers(t, h) {
		t.Run(c.name, func(t *testing.T) {
			out, err := c.call(t, "forward.fetch", map[string]any{
				"port": port, "method": "GET", "path": "/index.html",
			})
			if err != nil {
				t.Fatalf("forward.fetch: %v", err)
			}
			var resp forward.Response
			if err := json.Unmarshal(out, &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Status != 200 {
				t.Fatalf("status = %d", resp.Status)
			}
			if !strings.Contains(string(resp.Body), page) {
				t.Errorf("body = %q, want the page", string(resp.Body))
			}
			if !strings.Contains(string(resp.Body), "/index.html") {
				t.Error("the path did not reach the dev server")
			}
		})
	}
}

func isCode(err error, code int) bool { return codeOf(err) == code }

// codeOf normalizes both error shapes onto the wire code. A remote caller gets
// a *jsonrpc.Error; the in-process caller gets the handler's own error, which
// rpc.Code maps the same way every transport does.
func codeOf(err error) int {
	if err == nil {
		return 0
	}
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) {
		return rpcErr.Code
	}
	return rpc.Code(err)
}
