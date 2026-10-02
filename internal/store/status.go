package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/repogo/host/internal/agent"
)

// ManagerTurnPrefix keys a turn this host ran, as opposed to a hook's or a transcript's.
const ManagerTurnPrefix = "manager:"

// TurnObservation keys the whole request so delayed completions cannot end another turn.
type TurnObservation struct {
	Key        string
	StartedAt  *time.Time
	FinishedAt *time.Time

	// What the turn was sent with, so a row the transcript has not filled
	// yet can show its title and model from write 1 (see queries.go).
	Prompt string
	Model  string
	// The title a device started the chat with; it names the row ahead of
	// the prompt's opening words.
	Title string
	// Unlike the model, the permission is exact, so it always replaces the row's.
	PermissionMode agent.PermissionMode
}

// SetStatus is writes 1 and 4 in queries.go. It can precede the transcript;
// its sentinel fingerprint forces a reparse.
func (s *Store) SetStatus(id ChatID, cwd string, status agent.ChatStatus, at time.Time, turn TurnObservation) error {
	provider, sessionID, ok := id.split()
	if !ok || !status.Valid() || at.IsZero() {
		return fmt.Errorf("%w: chat status observation", ErrInvalid)
	}
	if turn.Key == "" && (turn.StartedAt != nil || turn.FinishedAt != nil) {
		return fmt.Errorf("%w: turn timing requires a request identity", ErrInvalid)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var key string
	var started, finished sql.NullInt64
	err = tx.QueryRow(`SELECT last_turn_key, last_turn_started_at, last_turn_finished_at
		FROM sessions WHERE agent = ? AND session_id = ?`, provider, sessionID).
		Scan(&key, &started, &finished)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	fresh := err == sql.ErrNoRows
	key, started, finished = mergeTurn(key, started, finished, turn)
	// A turn seen starting is a prompt just sent, from a device or a hook,
	// ahead of the transcript line the next sweep will read.
	var sent int64
	if turn.StartedAt != nil {
		sent = turn.StartedAt.UnixMilli()
	}
	// Claude's `default` is an alias the catalog names "Default (recommended)";
	// the transcript's model will say what it ran.
	model := turn.Model
	if model == "default" {
		model = ""
	}
	requested := turn.Title
	if requested == "" {
		requested = openingWords(turn.Prompt)
	}
	_, err = tx.Exec(statusUpsert,
		provider, sessionID, cwd, requested, model, string(turn.PermissionMode),
		at.UnixMilli(), at.UnixMilli(), status, at.UnixMilli(), key, started, finished, sent, s.rev.Add(1))
	if err != nil {
		return fmt.Errorf("store: update chat status: %w", err)
	}
	// A chat with no transcript yet is found by its title; the first sync
	// replaces the row with the title and text the transcript holds.
	var sid, syncedAt, searchHash int64
	var title string
	if err := tx.QueryRow(`SELECT sid, title, synced_at, search_hash FROM sessions WHERE agent = ? AND session_id = ?`,
		provider, sessionID).Scan(&sid, &title, &syncedAt, &searchHash); err != nil {
		return fmt.Errorf("store: update chat status: %w", err)
	}
	if syncedAt == 0 {
		if err := indexChat(tx, sid, title, nil, searchHash); err != nil {
			return err
		}
	}
	changed := []ChatID{id}
	if fresh {
		renamed, err := s.assignVoiceHandles(tx)
		if err != nil {
			return err
		}
		changed = append(changed, renamed...)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.notify(Change{Changed: changed})
	return nil
}

// InterruptOrphans settles every chat left active by a turn this host ran. Those
// turns end with the process, so at start nothing is behind such a status.
func (s *Store) InterruptOrphans(at time.Time) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	orphans, err := selectChats(tx, `SELECT agent, session_id FROM sessions WHERE status IN (`+marks(len(agent.ActiveStatuses))+`) AND last_turn_key LIKE ?`,
		append(anys(agent.ActiveStatuses), ManagerTurnPrefix+"%")...)
	if err != nil {
		return 0, err
	}
	for _, id := range orphans {
		kind, sessionID, _ := id.split()
		// The finish ends the turn's hold on the row, so a later turn from a
		// terminal can take it.
		if _, err := tx.Exec(`UPDATE sessions SET status = ?, status_at = MAX(status_at, ?),
			last_turn_finished_at = COALESCE(last_turn_finished_at, ?), rev = ? WHERE agent = ? AND session_id = ?`,
			agent.ChatInterrupted, at.UnixMilli(), at.UnixMilli(), s.rev.Add(1), kind, sessionID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	s.notify(Change{Changed: orphans})
	return len(orphans), nil
}

// mergeTurn folds one turn observation into the row. A live observer (hook or
// Manager) owns it until a newer turn, live or read off the file, displaces
// it; a transcript reading fills in until then.
func mergeTurn(key string, started, finished sql.NullInt64, turn TurnObservation) (string, sql.NullInt64, sql.NullInt64) {
	if turn.Key == "" || (turn.StartedAt == nil && turn.FinishedAt == nil) {
		return key, started, finished
	}
	fromFile := strings.HasPrefix(turn.Key, transcriptTurnPrefix)
	rowFromFile := strings.HasPrefix(key, transcriptTurnPrefix)
	if fromFile {
		// A live turn keeps the row until the file shows one begun after it
		// ended: a turn run outside this host with no hook to report it.
		after := turn.StartedAt != nil && finished.Valid && turn.StartedAt.UnixMilli() > finished.Int64
		if key != "" && !rowFromFile && !after {
			return key, started, finished
		}
		// The file is reread whole, so its reading replaces its last one.
		started, finished = sql.NullInt64{}, sql.NullInt64{}
		if turn.StartedAt != nil {
			started = sql.NullInt64{Int64: turn.StartedAt.UnixMilli(), Valid: true}
		}
		if turn.FinishedAt != nil {
			finished = sql.NullInt64{Int64: turn.FinishedAt.UnixMilli(), Valid: true}
		}
		return turn.Key, started, finished
	}
	if key != turn.Key {
		// The agent this host runs fires its own prompt hook once it has
		// started, so a hook during a live host turn is that turn, seen late.
		hostTurnLive := strings.HasPrefix(key, ManagerTurnPrefix) && !finished.Valid
		if hostTurnLive && !strings.HasPrefix(turn.Key, ManagerTurnPrefix) {
			return key, started, finished
		}
		boundary := started
		if !boundary.Valid {
			boundary = finished
		}
		newer := turn.StartedAt != nil && (!boundary.Valid || turn.StartedAt.UnixMilli() > boundary.Int64)
		switch {
		case key == "" || newer:
			key, started, finished = turn.Key, sql.NullInt64{}, sql.NullInt64{}
		case rowFromFile:
			// Same turn seen live: the exact stamps win where given, and a
			// prompt stamp means a new turn, so the file's finish is stale.
			key = turn.Key
			if turn.StartedAt != nil {
				started, finished = sql.NullInt64{}, sql.NullInt64{}
			}
		default:
			return key, started, finished
		}
	}
	if turn.StartedAt != nil && !started.Valid {
		started = sql.NullInt64{Int64: turn.StartedAt.UnixMilli(), Valid: true}
	}
	if turn.FinishedAt != nil && (!finished.Valid || turn.FinishedAt.UnixMilli() > finished.Int64) {
		finished = sql.NullInt64{Int64: turn.FinishedAt.UnixMilli(), Valid: true}
	}
	return key, started, finished
}
