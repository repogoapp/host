package cursor

import (
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/apphome"
)

// turnTime is when a host-run turn started and ended, in Unix milliseconds.
// Cursor's native store keeps no timestamps, so without these a replayed
// reply has nothing for the phone's "Worked for" label.
type turnTime struct {
	StartedAt int64 `json:"startedAt"`
	EndedAt   int64 `json:"endedAt"`
}

func (s *Sessions) timesPath(id string) (string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return "", err
	}
	if s.provider.deps.Root != "" {
		return filepath.Join(s.provider.deps.Root, "cursor-turn-times", id+".json"), nil
	}
	return apphome.Path("cursor-turn-times", id+".json")
}

func (s *Sessions) readTimes(id string) (map[string]turnTime, error) {
	path, err := s.timesPath(id)
	if err != nil {
		return nil, err
	}
	times := map[string]turnTime{}
	if _, err = apphome.ReadJSON(path, &times); err != nil {
		return nil, err
	}
	return times, nil
}

func (s *Sessions) saveTurnTime(id, turn string, t turnTime) error {
	s.timesMu.Lock()
	defer s.timesMu.Unlock()
	times, err := s.readTimes(id)
	if err != nil {
		return err
	}
	times[turn] = t
	path, err := s.timesPath(id)
	if err != nil {
		return err
	}
	return apphome.WriteJSON(path, times, 0o600)
}

func (s *Sessions) deleteTimes(id string) error {
	s.timesMu.Lock()
	defer s.timesMu.Unlock()
	path, err := s.timesPath(id)
	if err != nil {
		return err
	}
	if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// restoreTimes stamps replayed events with the times their turn ran on this
// host. Turns run elsewhere keep a zero time.
func (s *Sessions) restoreTimes(id string, events []agent.Event) {
	s.timesMu.Lock()
	times, err := s.readTimes(id)
	s.timesMu.Unlock()
	if err != nil {
		s.provider.deps.Log.Warn("Cursor turn times unreadable", "session", id, "error", err)
		return
	}
	for i, e := range events {
		t, ok := times[e.TurnID]
		if !ok || e.At != 0 {
			continue
		}
		switch e.Kind {
		case agent.EventTurnStarted, agent.EventUserMessage:
			events[i].At = t.StartedAt
		case agent.EventTurnFinished, agent.EventTurnFailed:
			events[i].At = t.EndedAt
		}
	}
}
