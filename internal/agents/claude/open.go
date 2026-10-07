package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/repogo/host/internal/agent"
)

// openSession is a Claude session running in a terminal or the desktop app,
// as Claude registers it in <home>/sessions/<pid>.json.
type openSession struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	Entrypoint string `json:"entrypoint"`
	Socket     string `json:"messagingSocketPath"`
}

// findOpenSession returns the session open outside the host for sessionID.
// The host's own processes register as "sdk-cli" and are skipped.
func (r *runner) findOpenSession(sessionID string) (openSession, bool) {
	if sessionID == "" {
		return openSession{}, false
	}
	files, _ := filepath.Glob(filepath.Join(r.home, "sessions", "*.json"))
	for _, file := range files {
		open, err := readOpenSession(file)
		if err != nil || open.SessionID != sessionID || open.Entrypoint == "sdk-cli" || open.Socket == "" {
			continue
		}
		if syscall.Kill(open.PID, 0) == nil {
			return open, true
		}
	}
	return openSession{}, false
}

func readOpenSession(file string) (openSession, error) {
	var open openSession
	data, err := os.ReadFile(file)
	if err != nil {
		return open, err
	}
	return open, json.Unmarshal(data, &open)
}

// sendOpen hands the prompt to the open session's inbox socket and ends the
// host's turn: the session runs it as a terminal turn, which its hooks stream
// and its transcript records, so the phone shows it once.
func (r *runner) sendOpen(ctx context.Context, open openSession, req agent.TurnRequest, io agent.TurnIO) (agent.Result, error) {
	result := agent.Result{SessionID: open.SessionID}
	if io.Session != nil {
		io.Session(open.SessionID)
	}
	var lines [][]byte
	if token := r.openSessionToken(open.PID); token != "" {
		auth, _ := json.Marshal(struct {
			Type  string `json:"type"`
			Token string `json:"token"`
		}{"auth", token})
		lines = append(lines, auth)
	}
	// The inbox drops content blocks, so the prompt goes as text, with
	// attachments as the file links Claude reads them from.
	var text []string
	for _, block := range userMessage(req, open.SessionID).Message.Content {
		if block.Type == "text" {
			text = append(text, block.Text)
		}
	}
	type inboxMessage struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	message, _ := json.Marshal(struct {
		Type    string       `json:"type"`
		Message inboxMessage `json:"message"`
	}{"user", inboxMessage{"user", strings.Join(text, "\n")}})
	lines = append(lines, message)

	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", open.Socket)
	if err != nil {
		return result, fmt.Errorf("reach the open Claude session: %w", err)
	}
	defer conn.Close()
	for _, line := range lines {
		if _, err := conn.Write(append(line, '\n')); err != nil {
			return result, fmt.Errorf("send to the open Claude session: %w", err)
		}
	}
	r.deps.Log.Info("Claude prompt sent to the session open outside the host", "chat", req.ChatID, "pid", open.PID, "entrypoint", open.Entrypoint)
	result.StopReason = "end_turn"
	return result, nil
}

// openSessionToken is the inbox key the session publishes beside its
// registration, so a peer can authenticate.
func (r *runner) openSessionToken(pid int) string {
	keys, _ := filepath.Glob(filepath.Join(r.home, "sessions", fmt.Sprintf("%d.*.key", pid)))
	if len(keys) == 0 {
		return ""
	}
	data, err := os.ReadFile(keys[0])
	if err != nil {
		return ""
	}
	var key struct {
		PeerToken string `json:"peerToken"`
	}
	if json.Unmarshal(data, &key) != nil {
		return ""
	}
	return key.PeerToken
}
