package chatsync

import (
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/notify"
	"github.com/repogo/host/internal/store"
)

// ObserveHook commits chat metadata before the bridge acknowledges its drop.
func (s *Syncer) ObserveHook(notice notify.Notice) error {
	id := store.ChatID(notice.ChatID())
	// Codex's hooks name the model before its rollout does; Claude's do not,
	// and the transcript brings it.
	timing := store.TurnObservation{Model: notice.Model}
	if source, turnID := notice.Turn(); turnID != "" {
		timing.Key = source + ":" + turnID
		switch {
		case notice.Starts():
			timing.StartedAt = &notice.At
			timing.Prompt = notice.Prompt
		case notice.Ends():
			timing.FinishedAt = &notice.At
		}
	}
	// The notice keeps the turn's outcome for turn history; the row shows
	// where the chat stands now, which after Stop is idle.
	if err := s.db.SetStatus(id, notice.Cwd, notice.Status.AtRest(), notice.At, timing); err != nil {
		return err
	}
	if notice.Starts() {
		// Continuing a resolved chat reopens it; the transcript-derived
		// version of this rule lives in store.SyncBatch for Codex.
		if _, err := s.db.UnresolveIfActive(id, notice.At); err != nil {
			return err
		}
	}
	s.Refresh(id)
	return nil
}

// ObserveTurn records a turn this host runs, so lists learn it started or
// ended now rather than at the next sweep.
func (s *Syncer) ObserveTurn(turn agent.TurnStatus, status agent.ChatStatus, at time.Time) {
	id := store.ChatID(agent.ChatID(turn.Agent, turn.SessionID))
	timing := store.TurnObservation{Key: store.ManagerTurnPrefix + turn.TurnID, StartedAt: turn.StartedAt, FinishedAt: turn.EndedAt,
		Prompt: turn.Prompt, Model: turn.Model, PermissionMode: turn.PermissionMode, Title: turn.Title}
	if err := s.db.SetStatus(id, turn.Cwd, status.AtRest(), at, timing); err != nil {
		s.log.Warn("chat status update failed", "chat", id, "err", err)
		return
	}
	if turn.EndedAt != nil && turn.Title != "" && turn.SessionID != "" {
		s.keepStartTitle(id, turn)
	}
	s.Refresh(id)
}

// keepStartTitle writes the title a new chat was started with into the
// agent's files once its first turn has made them, so the CLI pickers and a
// rebuilt cache show it too. A rename during that turn is the user's and stays.
func (s *Syncer) keepStartTitle(id store.ChatID, turn agent.TurnStatus) {
	chat, err := s.db.Info(id)
	if err != nil || chat.Title != turn.Title {
		return
	}
	if err := s.sessions.Rename(turn.SessionID, turn.Title); err != nil {
		s.log.Warn("chat title write failed", "chat", id, "err", err)
	}
}

// Refresh asks for a sweep now, so the transcript behind a status just
// written is read before the periodic one would get to it.
func (s *Syncer) Refresh(store.ChatID) {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
