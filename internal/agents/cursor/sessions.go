package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/apphome"
	"github.com/repogo/host/internal/cursoragent"
	"github.com/repogo/host/internal/session"
)

// Snapshots are disposable; Cursor's SQLite database remains the conversation's source of truth.
type Sessions struct {
	provider *Provider
	dir      string
	mu       sync.Mutex
	cache    map[string]*snapshotCache
	timesMu  sync.Mutex
}
type snapshotCache struct {
	mu       sync.Mutex
	size     int64
	modified time.Time
	events   []agent.Event
}
type nativeMeta struct {
	SchemaVersion int    `json:"schemaVersion"`
	Cwd           string `json:"cwd"`
	Title         string `json:"title,omitempty"`
}

func (*Sessions) Kind() agent.Kind { return agent.KindCursor }
func (s *Sessions) directory(id string) (string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", fmt.Errorf("invalid Cursor session id: %w", err)
	}
	return filepath.Join(s.dir, id), nil
}
func (s *Sessions) meta(id string) (session.Meta, error) {
	dir, err := s.directory(id)
	if err != nil {
		return session.Meta{}, err
	}
	var native nativeMeta
	found, err := apphome.ReadJSON(filepath.Join(dir, "meta.json"), &native)
	if err != nil {
		return session.Meta{}, err
	}
	if !found || native.Cwd == "" {
		return session.Meta{}, session.ErrNotFound
	}
	m := session.Meta{ID: id, Agent: agent.KindCursor, Cwd: native.Cwd, Title: native.Title, Path: filepath.Join(dir, "store.db")}
	for _, name := range []string{"store.db", "store.db-wal", "meta.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if os.IsNotExist(err) && name == "store.db-wal" {
			continue
		}
		if err != nil {
			return m, err
		}
		m.SizeBytes += info.Size()
		if info.ModTime().After(m.UpdatedAt) {
			m.UpdatedAt = info.ModTime()
		}
	}
	return m, nil
}
func (s *Sessions) List() ([]session.Meta, error) {
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []session.Meta{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := uuid.Parse(entry.Name()); err != nil {
			continue
		}
		m, err := s.meta(entry.Name())
		if errors.Is(err, session.ErrNotFound) || os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if live, ok := s.provider.pool.Get(agent.ChatID(agent.KindCursor, m.ID)); ok {
			live.mu.Lock()
			m.Model = live.state.Models.CurrentModelID
			m.Settings.Mode = live.state.Modes.CurrentModeID
			live.mu.Unlock()
			s.provider.pool.Release(live.key, live)
		}
		out = append(out, m)
	}
	return out, nil
}
func (*Sessions) ParseLine([]byte) ([]agent.Event, string) { return nil, "" }

func (s *Sessions) Snapshot(m session.Meta) ([]agent.Event, error) {
	if s.provider.deps.Context == nil {
		return nil, errors.New("Cursor snapshots require host context")
	}
	ctx, cancel := context.WithTimeout(s.provider.deps.Context, 60*time.Second)
	defer cancel()
	return s.snapshot(ctx, m)
}
func (s *Sessions) snapshot(ctx context.Context, m session.Meta) ([]agent.Event, error) {
	if live, ok := s.provider.pool.Get(agent.ChatID(agent.KindCursor, m.ID)); ok {
		live.mu.Lock()
		use := !live.closed && (live.io != nil || (live.nativeSize == m.SizeBytes && live.nativeTime.Equal(m.UpdatedAt)))
		events := normalizeHistory(live.history)
		live.mu.Unlock()
		s.provider.pool.Release(live.key, live)
		if use {
			return events, nil
		}
	}
	s.mu.Lock()
	cached := s.cache[m.ID]
	if cached == nil {
		cached = &snapshotCache{}
		s.cache[m.ID] = cached
	}
	s.mu.Unlock()
	cached.mu.Lock()
	defer cached.mu.Unlock()
	current, err := s.meta(m.ID)
	if err != nil {
		return nil, err
	}
	m = current
	if cached.events != nil && cached.size == m.SizeBytes && cached.modified.Equal(m.UpdatedAt) {
		return append([]agent.Event(nil), cached.events...), nil
	}
	replay := &liveSession{provider: s.provider, id: m.ID, cwd: m.Cwd, replaying: true}
	replay.resetToolsLocked()
	c, err := cursoragent.Start(ctx, cursoragent.Options{Executable: s.provider.executable(), Cwd: m.Cwd, Env: s.provider.environment()}, cursoragent.Handlers{Request: replay.respond, Notification: replay.handle})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if _, err = c.Initialize(ctx); err != nil {
		return nil, err
	}
	if _, err = c.Open(ctx, cursoragent.SessionParams{SessionID: m.ID, Cwd: m.Cwd}); err != nil {
		return nil, err
	}
	replay.mu.Lock()
	replay.finishReplayLocked()
	cached.events = normalizeHistory(replay.history)
	replay.mu.Unlock()
	_ = c.Close()
	if current, err := s.meta(m.ID); err == nil {
		m = current
	}
	cached.size = m.SizeBytes
	cached.modified = m.UpdatedAt
	return append([]agent.Event(nil), cached.events...), nil
}
func normalizeHistory(events []agent.Event) []agent.Event {
	out := make([]agent.Event, 0, len(events))
	for i := 0; i < len(events); {
		e := events[i]
		i++
		if e.Kind == agent.EventText || e.Kind == agent.EventReasoning {
			var text strings.Builder
			text.WriteString(e.Text)
			for i < len(events) && events[i].Kind == e.Kind && events[i].TurnID == e.TurnID {
				text.WriteString(events[i].Text)
				i++
			}
			e.Text = text.String()
		}
		e.Seq = uint64(len(out) + 1)
		out = append(out, e)
	}
	return out
}
func (s *Sessions) Watch(ctx context.Context, m session.Meta) <-chan agent.Event {
	out := make(chan agent.Event, 256)
	go func() {
		defer close(out)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		var previous []agent.Event
		for {
			fresh, err := s.meta(m.ID)
			if err == nil {
				read, cancel := context.WithTimeout(ctx, 60*time.Second)
				events, err := s.snapshot(read, fresh)
				cancel()
				if err == nil {
					for i, e := range events {
						if i < len(previous) && reflect.DeepEqual(e, previous[i]) {
							continue
						}
						select {
						case out <- e:
						case <-ctx.Done():
							return
						}
					}
					previous = events
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return out
}
func (s *Sessions) Rename(m session.Meta, title string) error {
	dir, err := s.directory(m.ID)
	if err != nil {
		return err
	}
	// Preserve native metadata fields introduced by a newer CLI.
	var native map[string]json.RawMessage
	found, err := apphome.ReadJSON(filepath.Join(dir, "meta.json"), &native)
	if err != nil {
		return err
	}
	if !found {
		return session.ErrNotFound
	}
	native["title"], err = json.Marshal(title)
	if err != nil {
		return err
	}
	return apphome.WriteJSON(filepath.Join(dir, "meta.json"), native, 0o600)
}
func (s *Sessions) Delete(m session.Meta) error {
	dir, err := s.directory(m.ID)
	if err != nil {
		return err
	}
	key := agent.ChatID(agent.KindCursor, m.ID)
	if live, ok := s.provider.pool.Get(key); ok {
		s.provider.pool.Retire(key, live)
	}
	s.mu.Lock()
	cached := s.cache[m.ID]
	if cached == nil {
		cached = &snapshotCache{}
		s.cache[m.ID] = cached
	}
	s.mu.Unlock()
	cached.mu.Lock()
	defer cached.mu.Unlock()
	cached.events = nil
	if err = s.deleteTimes(m.ID); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
