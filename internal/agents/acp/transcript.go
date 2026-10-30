package acp

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
)

// An ACP agent keeps no history a client can read back, so the host writes
// each chat's own: one JSON line per record, appended, never rewritten.
const (
	// lineChat opens the file: which project agent the chat runs, and where.
	lineChat = "chat"
	// lineSession is the agent's own session id, for session/load after a restart.
	lineSession = "session"
	// lineTitle is a rename; the newest wins.
	lineTitle = "title"
	// lineModel is the model a turn runs on, by id and display name.
	lineModel = "model"
	lineEvent = "event"
)

type line struct {
	Type string `json:"type"`
	At   string `json:"at"`

	Agent     string       `json:"agent,omitempty"`
	Cwd       string       `json:"cwd,omitempty"`
	Session   string       `json:"session,omitempty"`
	Title     string       `json:"title,omitempty"`
	Model     string       `json:"model,omitempty"`
	ModelName string       `json:"model_name,omitempty"`
	Event     *agent.Event `json:"event,omitempty"`
}

// transcript is one chat's file; a turn's events and the agent's updates
// write from different goroutines.
type transcript struct {
	mu   sync.Mutex
	path string
}

// chatInfo is what a transcript says about its chat, read back.
type chatInfo struct {
	Agent, Cwd, Session, Title string
	// Model and ModelName are the newest turn's.
	Model, ModelName string
	ModelAt          time.Time
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// createTranscript starts a chat's file with its chat line.
func createTranscript(path, agentID, cwd string) (*transcript, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	t := &transcript{path: path}
	return t, t.append(line{Type: lineChat, Agent: agentID, Cwd: cwd})
}

func (t *transcript) append(l line) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if l.At == "" {
		l.At = now()
	}
	return session.AppendLine(t.path, l)
}

func (t *transcript) event(e agent.Event) error {
	return t.append(line{Type: lineEvent, Event: &e})
}

// scan calls fn with each line of path it can read.
func scan(path string, fn func(line)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), session.LineLimit)
	for sc.Scan() {
		var l line
		if json.Unmarshal(sc.Bytes(), &l) == nil {
			fn(l)
		}
	}
	return sc.Err()
}

// readInfo reads a transcript's chat line, newest session and newest title.
func readInfo(path string) (chatInfo, error) {
	var info chatInfo
	err := scan(path, func(l line) {
		switch l.Type {
		case lineChat:
			info.Agent, info.Cwd = l.Agent, l.Cwd
		case lineSession:
			info.Session = l.Session
		case lineTitle:
			info.Title = l.Title
		case lineModel:
			info.Model, info.ModelName = l.Model, l.ModelName
			info.ModelAt, _ = time.Parse(time.RFC3339Nano, l.At)
		}
	})
	return info, err
}

// readEvents is every event a transcript recorded, in order.
func readEvents(path string) ([]agent.Event, error) {
	var out []agent.Event
	err := scan(path, func(l line) {
		if l.Type == lineEvent && l.Event != nil {
			out = append(out, *l.Event)
		}
	})
	return out, err
}
