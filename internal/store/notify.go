package store

import (
	"time"

	"github.com/repogoapp/host/internal/agent"
)

// Change is what one committed write did to the chat list: rows that read
// differently now, and rows that are gone.
type Change struct {
	Changed []ChatID
	Removed []ChatID
}

// Notify sets the one listener told after each commit that moved the chat
// list, on the writer's goroutine. Set before the first write; a change made
// while no listener is set is not replayed.
func (s *Store) Notify(fn func(Change)) { s.onChange = fn }

func (s *Store) notify(c Change) {
	if s.onChange != nil && (len(c.Changed) > 0 || len(c.Removed) > 0) {
		s.onChange(c)
	}
}

// StatusChange is a chat's status as SetStatus or an interrupt recorded it, for
// the agent clock. An interrupt's At is the turn's last sign of life.
type StatusChange struct {
	Chat    ChatID
	Cwd     string
	Status  agent.ChatStatus
	At      time.Time
	TurnKey string
}

// OnStatus sets the one listener told each recorded status, after its commit,
// on the writer's goroutine. Set before the first write.
func (s *Store) OnStatus(fn func(StatusChange)) { s.onStatus = fn }

func (s *Store) reportStatus(c StatusChange) {
	if s.onStatus != nil {
		s.onStatus(c)
	}
}

// LastSeen is a chat's last sign of life: its latest status or file write.
func (s *Store) LastSeen(id ChatID) (time.Time, bool) {
	kind, sessionID, ok := id.split()
	if !ok {
		return time.Time{}, false
	}
	var at int64
	if err := s.db.QueryRow(`SELECT MAX(status_at, updated_at) FROM sessions WHERE agent = ? AND session_id = ?`,
		kind, sessionID).Scan(&at); err != nil || at == 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(at), true
}
