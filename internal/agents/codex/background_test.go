package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/codexappserver"
)

// fakeBackground serves the fake app server on a background-server socket
// under home, one fake process per connection, and records every frame the
// host sends.
func fakeBackground(t *testing.T, home string) *[]frame {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	socket := codexappserver.SocketPath(home)
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var sent []frame
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		cmd := exec.Command(executable)
		cmd.Env = append(os.Environ(), "REPOGO_FAKE_CODEX=1")
		stdin, _ := cmd.StdinPipe()
		stdout, _ := cmd.StdoutPipe()
		if cmd.Start() != nil {
			return
		}
		defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
		go func() {
			lines := bufio.NewScanner(stdout)
			lines.Buffer(make([]byte, 64<<10), 1<<20)
			for lines.Scan() {
				_ = ws.Write(context.Background(), websocket.MessageText, lines.Bytes())
			}
		}()
		for {
			_, data, err := ws.Read(r.Context())
			if err != nil {
				return
			}
			var f frame
			_ = json.Unmarshal(data, &f)
			sent = append(sent, f)
			_, _ = stdin.Write(append(data, '\n'))
		}
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return &sent
}

// sharedRunner is a test runner whose chats join a fake background server.
func sharedRunner(t *testing.T) (*runner, *[]frame) {
	t.Helper()
	r := testRunner(t)
	// A short home: a Unix socket's path has a small length limit.
	home, err := os.MkdirTemp("/tmp", "cx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	sent := fakeBackground(t, home)
	r.background = &background{home: home, executable: r.executable, log: r.deps.Log}
	r.tokens = &toolTokens{path: filepath.Join(t.TempDir(), "tokens.json")}
	r.deps.ProjectEnv = func(string) []string { return []string{"AWS_PROFILE=work"} }
	return r, sent
}

func TestChatJoinsTheBackgroundServer(t *testing.T) {
	r, sent := sharedRunner(t)
	result, text, _, err := turnOf(t, r, agent.TurnRequest{ChatID: "new", Cwd: t.TempDir(), Prompt: "hello"}, nil)
	if err != nil || text != "hello" {
		t.Fatalf("turn said %q: %v", text, err)
	}
	key := agent.ChatID(agent.KindCodex, result.SessionID)
	s, ok := r.pool.Get(key)
	if !ok {
		t.Fatal("no session for the chat")
	}
	r.pool.Release(key, s)
	if !s.shared {
		t.Fatal("chat ran on its own app server with a background server running")
	}
	// Its commands get the project's variables through the thread's config.
	for _, f := range *sent {
		if f.Method == "thread/start" {
			if !strings.Contains(string(f.Params), `"shell_environment_policy":{"set":{"AWS_PROFILE":"work"}}`) {
				t.Fatalf("thread/start without the project's env: %s", f.Params)
			}
			return
		}
	}
	t.Fatal("no thread/start sent")
}

func TestHookFromAppTurnOnBackgroundServerIsOwned(t *testing.T) {
	r, _ := sharedRunner(t)
	if _, _, _, err := turnOf(t, r, agent.TurnRequest{ChatID: "new", Cwd: t.TempDir(), Prompt: "hello"}, nil); err != nil {
		t.Fatal(err)
	}
	if !r.ownsTurn("thread-new", "turn-1") {
		t.Fatal("RepoGo's own turn not recognized")
	}
	if r.ownsTurn("thread-new", "terminal-turn") {
		t.Fatal("a turn RepoGo never ran counted as its own")
	}
}

// Another client's turn asking for approval is left to that client.
func TestOthersRequestIsNotAnswered(t *testing.T) {
	s := newLiveSession(t.TempDir(), nil)
	s.shared = true
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := s.respond(ctx, codexappserver.Request{Method: codexappserver.MethodElicitation, Params: json.RawMessage(`{"mode":"url"}`)})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("answered another client's request: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("a withdrawn request should end without an answer")
	}
}

func TestToolTokensOutliveTheHost(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens.json")
	(&toolTokens{path: path}).put("thread", "secret")
	if got := (&toolTokens{path: path}).get("thread"); got != "secret" {
		t.Fatalf("token after restart = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("tokens file %v, %v", info, err)
	}
	old := &toolTokens{path: path}
	old.loadLocked()
	old.tokens["stale"] = tokenEntry{Token: "x", Used: time.Now().Add(-2 * tokenKeep)}
	old.put("thread", "secret")
	if (&toolTokens{path: path}).get("stale") != "" {
		t.Fatal("a token long unused was kept")
	}
}

func TestFeatureOn(t *testing.T) {
	list := []byte("app_server_websocket_compression         under development  false\ndaemon_auto_start                        stable             true\n")
	if !featureOn(list, "daemon_auto_start") || featureOn(list, "app_server_websocket_compression") || featureOn(list, "missing") {
		t.Fatal("misread codex features list")
	}
}

func TestShellEnv(t *testing.T) {
	if shellEnv(nil) != nil {
		t.Fatal("no variables should add no policy")
	}
	got, _ := json.Marshal(shellEnv([]string{"AWS_PROFILE=work", "EMPTY=", "bad"}))
	if string(got) != `{"set":{"AWS_PROFILE":"work","EMPTY":""}}` {
		t.Fatalf("policy %s", got)
	}
}
