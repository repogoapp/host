package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/repogo/host/internal/agent"
)

func writeOpenSession(t *testing.T, home string, open openSession) {
	t.Helper()
	data, _ := json.Marshal(open)
	if err := os.WriteFile(filepath.Join(home, "sessions", fmt.Sprintf("%d.json", open.PID)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFindOpenSessionSkipsTheHostsOwn(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "sessions"), 0o700)
	r := &runner{home: home}
	writeOpenSession(t, home, openSession{PID: os.Getpid(), SessionID: "s1", Entrypoint: "sdk-cli", Socket: "/tmp/x.sock"})
	if _, ok := r.findOpenSession("s1"); ok {
		t.Fatal("the host's own process was taken for an open session")
	}
	writeOpenSession(t, home, openSession{PID: os.Getpid(), SessionID: "s1", Entrypoint: "cli", Socket: "/tmp/x.sock"})
	if open, ok := r.findOpenSession("s1"); !ok || open.Entrypoint != "cli" {
		t.Fatalf("open session = %+v, %v", open, ok)
	}
	if _, ok := r.findOpenSession(""); ok {
		t.Fatal("a new chat matched an open session")
	}
}

func TestSendOpenDeliversToTheInbox(t *testing.T) {
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, "sessions"), 0o700)
	// A Unix socket path is capped near 104 bytes, past a macOS t.TempDir.
	dir, err := os.MkdirTemp("/tmp", "inbox")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	open := openSession{PID: os.Getpid(), SessionID: "s1", Entrypoint: "cli", Socket: socket}
	writeOpenSession(t, home, open)
	os.WriteFile(filepath.Join(home, "sessions", fmt.Sprintf("%d.abc.key", open.PID)), []byte(`{"peerToken":"tok"}`), 0o600)

	received := make(chan []string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		scanner := bufio.NewScanner(conn)
		var lines []string
		for len(lines) < 2 && scanner.Scan() {
			lines = append(lines, scanner.Text())
		}
		received <- lines
	}()

	r := &runner{deps: agent.Dependencies{Log: slog.New(slog.DiscardHandler)}, home: home}
	var session string
	result, err := r.sendOpen(t.Context(), open, agent.TurnRequest{ChatID: "claude:s1", SessionID: "s1", Prompt: "Run the tests"}, agent.TurnIO{Session: func(id string) { session = id }})
	if err != nil || result.StopReason != "end_turn" || session != "s1" {
		t.Fatalf("result = %+v, %v, session %q", result, err, session)
	}
	lines := <-received
	if lines[0] != `{"type":"auth","token":"tok"}` || lines[1] != `{"type":"user","message":{"role":"user","content":"Run the tests"}}` {
		t.Fatalf("inbox got %q", lines)
	}
}

func TestPeerPromptIsTheUsersMessage(t *testing.T) {
	line := `{"type":"user","promptId":"p2","isMeta":true,"origin":{"kind":"peer"},"message":{"role":"user","content":"Another Claude session sent a message:\nRun the tests\n\nThis came from another Claude session — not typed by your user."}}`
	events, _ := NewSessions(t.TempDir()).ParseLine([]byte(line))
	if len(events) != 1 || events[0].Kind != agent.EventUserMessage || events[0].Text != "Run the tests" || events[0].TurnID != "p2" {
		t.Fatalf("events = %+v", events)
	}
	meta := `{"type":"user","isMeta":true,"message":{"role":"user","content":"Continue from where you left off."}}`
	if events, _ := NewSessions(t.TempDir()).ParseLine([]byte(meta)); len(events) != 0 {
		t.Fatalf("CLI bookkeeping became %+v", events)
	}
}
