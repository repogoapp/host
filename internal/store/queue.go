package store

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/repogo/host/internal/agent"
)

// QueuedTurn is one turn waiting in a chat's queue as chats.queue shows it:
// the full prompt for its bubble and Edit, and its files' paths on this host,
// which a device draws through fs.read as it does a sent message's.
type QueuedTurn struct {
	TurnID      string             `json:"turn_id"`
	Prompt      string             `json:"prompt"`
	QueuedAt    time.Time          `json:"queued_at"`
	Attachments []agent.Attachment `json:"attachments" wire:"array"`
}

// Queue is a chat's queued turns, oldest first, and the version a device
// compares with the row's queue_rev to know whether it holds these.
type Queue struct {
	QueueRev int64        `json:"queue_rev"`
	Queued   []QueuedTurn `json:"queued" wire:"array"`
}

// SaveQueue is the one writer of state.queued_turns and state.chat_queues: it
// replaces a chat's queued turns with queued, moves its queue_rev, then fans
// the chat out so its row carries the new count and version.
func (s *Store) SaveQueue(chatID string, queued []agent.StoredTurn) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM state.queued_turns WHERE chat_id = ?`, chatID); err != nil {
		return fmt.Errorf("store: save queue: %w", err)
	}
	for position, turn := range queued {
		request, err := json.Marshal(turn.Request)
		if err != nil {
			return fmt.Errorf("store: save queue: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO state.queued_turns (turn_id, chat_id, position, queued_at, held, request)
			VALUES (?, ?, ?, ?, ?, ?)`,
			turn.TurnID, chatID, position, turn.QueuedAt.UnixMilli(), turn.Held, request); err != nil {
			return fmt.Errorf("store: save queue: %w", err)
		}
	}
	if _, err := tx.Exec(`INSERT INTO state.chat_queues (chat_id, rev) VALUES (?, 1)
		ON CONFLICT (chat_id) DO UPDATE SET rev = rev + 1`, chatID); err != nil {
		return fmt.Errorf("store: save queue: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// A new chat's queue under a temporary id has no row to carry it yet.
	if _, _, ok := ChatID(chatID).split(); !ok {
		return nil
	}
	return s.touch(ChatID(chatID))
}

// LoadQueues is every chat's queued turns in order, for the Manager to restore.
func (s *Store) LoadQueues() (map[string][]agent.StoredTurn, error) {
	rows, err := s.db.Query(`SELECT chat_id, turn_id, queued_at, held, request FROM state.queued_turns
		ORDER BY chat_id, position`)
	if err != nil {
		return nil, fmt.Errorf("store: load queues: %w", err)
	}
	defer rows.Close()
	out := map[string][]agent.StoredTurn{}
	for rows.Next() {
		var chatID string
		var queuedAt int64
		var request []byte
		var turn agent.StoredTurn
		if err := rows.Scan(&chatID, &turn.TurnID, &queuedAt, &turn.Held, &request); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(request, &turn.Request); err != nil {
			return nil, fmt.Errorf("store: load queues: turn %s: %w", turn.TurnID, err)
		}
		turn.QueuedAt = time.UnixMilli(queuedAt)
		out[chatID] = append(out[chatID], turn)
	}
	return out, rows.Err()
}

// Queue is one chat's queued turns as chats.queue answers, with its queue_rev.
func (s *Store) Queue(id ChatID) (Queue, error) {
	if _, _, err := id.parts(); err != nil {
		return Queue{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Queue{}, err
	}
	defer tx.Rollback()

	out := Queue{Queued: []QueuedTurn{}}
	err = tx.QueryRow(`SELECT COALESCE((SELECT rev FROM state.chat_queues WHERE chat_id = ?), 0)`, string(id)).
		Scan(&out.QueueRev)
	if err != nil {
		return Queue{}, fmt.Errorf("store: read queue: %w", err)
	}
	rows, err := tx.Query(`SELECT turn_id, queued_at, request FROM state.queued_turns
		WHERE chat_id = ? ORDER BY position`, string(id))
	if err != nil {
		return Queue{}, fmt.Errorf("store: read queue: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var turn QueuedTurn
		var queuedAt int64
		var request []byte
		if err := rows.Scan(&turn.TurnID, &queuedAt, &request); err != nil {
			return Queue{}, err
		}
		var req agent.TurnRequest
		if err := json.Unmarshal(request, &req); err != nil {
			return Queue{}, fmt.Errorf("store: read queue: turn %s: %w", turn.TurnID, err)
		}
		turn.QueuedAt = time.UnixMilli(queuedAt)
		turn.Prompt, turn.Attachments = req.Prompt, req.Attachments
		if turn.Attachments == nil {
			turn.Attachments = []agent.Attachment{}
		}
		out.Queued = append(out.Queued, turn)
	}
	return out, rows.Err()
}
