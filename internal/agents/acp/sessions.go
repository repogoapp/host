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
	peeks *session.PeekCache[chatInfo]
}

func newSessions(dir string) *Sessions {
	return &Sessions{dir: dir, peeks: session.NewPeekCache[chatInfo]()}
}

func (s *Sessions) Kind() agent.Kind { return agent.KindProject }

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
			ID: strings.TrimSuffix(filepath.Base(f.Path), ".jsonl"), Agent: agent.KindProject,
			Cwd: infos[i].Cwd, Title: infos[i].Title, Path: f.Path, ProjectAgent: infos[i].Agent,
			Model: infos[i].Model, ModelLabel: infos[i].ModelName, ModelAt: infos[i].ModelAt,
			UpdatedAt: f.Info.ModTime(), SizeBytes: f.Info.Size(),
		})
	}
	return out, nil
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

// Rename records the user's title in the chat's own file.
func (s *Sessions) Rename(m session.Meta, title string) error {
	return (&transcript{path: m.Path}).append(line{Type: lineTitle, Title: title})
}
