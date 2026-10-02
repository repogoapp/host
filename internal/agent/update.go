package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/repogo/host/internal/errkind"
)

var ErrUpdating = errkind.New(errkind.Unavailable, "host update unavailable")

// GuardUpdate closes admission under the same lock as Send.
func (m *Manager) GuardUpdate(enabled, force bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !enabled {
		m.updateUntil = time.Time{}
		return nil
	}
	if time.Now().Before(m.updateUntil) {
		return fmt.Errorf("%w: another update is in progress", ErrUpdating)
	}
	if !force {
		for chat := range m.running {
			return fmt.Errorf("%w: chat %s is active; wait or use update --now", ErrUpdating, chat)
		}
	}
	// A killed installer must not leave the host refusing turns indefinitely.
	m.updateUntil = time.Now().Add(time.Minute)
	return nil
}

// ActiveChats is how many chats have a turn running or queued.
func (m *Manager) ActiveChats() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.running)
}

// StopAll ends every running turn and waits for them to settle. Queued turns
// are held first, so stopping a running turn cannot start the next, and they
// wait in state.db for the restarted host.
func (m *Manager) StopAll(ctx context.Context) error {
	m.queueMu.Lock()
	m.mu.Lock()
	queues := map[string][]StoredTurn{}
	for chatID, q := range m.pending {
		for _, t := range q {
			t.held = true
		}
		queues[chatID] = m.queuedLocked(chatID)
	}
	var running []string
	for _, t := range m.running {
		running = append(running, t.snapshot().TurnID)
	}
	m.mu.Unlock()
	for chatID, queue := range queues {
		m.saveQueue(chatID, queue)
	}
	m.queueMu.Unlock()
	for _, id := range running {
		if err := m.Stop(id); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
	}
	for {
		m.mu.Lock()
		active, ended := len(m.running), m.ended
		m.mu.Unlock()
		if active == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("turns are still stopping: %w", ctx.Err())
		case <-ended:
		}
	}
}
