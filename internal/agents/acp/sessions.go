package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
)

// Sessions reads the transcripts this host writes for ACP chats, one file
// per chat named by its id.
type Sessions struct {
	dir   string
	kind  agent.Kind
	peeks *session.PeekCache[chatInfo]
}

func newSessions(dir string, kind agent.Kind) *Sessions {
	return &Sessions{dir: dir, kind: kind, peeks: session.NewPeekCache[chatInfo]()}
}

func (s *Sessions) Kind() agent.Kind { return s.kind }

func (s *Sessions) path(id string) string { return filepath.Join(s.dir, id+".jsonl") }

func (s *Sessions) List() ([]session.Meta, error) {
	files, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var peeks []session.PeekFile
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
			continue
		}
		if info, err := f.Info(); err == nil {
			peeks = append(peeks, session.PeekFile{Path: filepath.Join(s.dir, f.Name()), Info: info})
		}
	}
	infos := session.PeekEach(peeks, s.peek)
	out := make([]session.Meta, 0, len(peeks))
	for i, f := range peeks {
		if infos[i].Cwd == "" {
			continue
		}
		out = append(out, session.Meta{
			ID: strings.TrimSuffix(filepath.Base(f.Path), ".jsonl"), Agent: s.kind,
			Cwd: infos[i].Cwd, Title: infos[i].Title, Path: f.Path, ProjectAgent: s.projectAgent(infos[i].Agent),
			Model: infos[i].Model, ModelLabel: infos[i].ModelName, ModelAt: infos[i].ModelAt,
			UpdatedAt: f.Info.ModTime(), SizeBytes: f.Info.Size(),
		})
	}
	return out, nil
}

// projectAgent is which of the environment's agents a chat runs; a shipped
// agent's chats name none.
func (s *Sessions) projectAgent(recorded string) string {
	if s.kind != agent.KindProject {
		return ""
	}
	return recorded
}

func (s *Sessions) peek(path string, info os.FileInfo) chatInfo {
	if c, ok := s.peeks.Get(path, info); ok {
		return c
	}
	c, _ := readInfo(path)
	s.peeks.Put(path, info, c)
	return c
}

// ParseLine reads back the events a turn recorded; every other line is the
// chat's own bookkeeping.
func (s *Sessions) ParseLine(raw []byte) ([]agent.Event, string) {
	var l line
	if json.Unmarshal(raw, &l) != nil || l.Type != lineEvent || l.Event == nil {
		return nil, ""
	}
	return []agent.Event{*l.Event}, l.At
}

// Link is a chat of the host's as the agent knows it: the host's id for the
// chat and the user's title for it, if any.
type Link struct {
	Chat, Title string
}

// Links is every host chat by the agent's session id for it, for a reader
// of the agent's own history to leave out the chats the host keeps, or to
// take their titles.
func (s *Sessions) Links() map[string]Link {
	files, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	out := map[string]Link{}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
			continue
		}
		info, err := f.Info()
		if err != nil {
			continue
		}
		c := s.peek(filepath.Join(s.dir, f.Name()), info)
		if c.Session != "" {
			out[c.Session] = Link{Chat: strings.TrimSuffix(f.Name(), ".jsonl"), Title: c.Title}
		}
	}
	return out
}

// Adopt starts the host's file for a chat the agent began on its own, named
// by the agent's session id, which the file records as its session. A chat
// adopted before keeps its file.
func (s *Sessions) Adopt(session, cwd string) (string, error) {
	path := s.path(session)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	file, err := createTranscript(path, string(s.kind), cwd)
	if err != nil {
		return "", err
	}
	return path, file.append(line{Type: lineSession, Session: session})
}

// Rename records the user's title in the chat's own file.
func (s *Sessions) Rename(m session.Meta, title string) error {
	return (&transcript{path: m.Path}).append(line{Type: lineTitle, Title: title})
}
