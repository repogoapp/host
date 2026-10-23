package store

import (
	"database/sql"
	"fmt"
	"slices"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
)

// Subagent is one row of subagents: work a chat's call started in files of
// its own.
type Subagent struct {
	Key    string
	CallID string
	Kind   string
	Name   string

	RunID  string
	Label  string
	Phase  string
	Order  int
	Done   bool
	Result string

	StartedAt int64
	UpdatedAt int64
}

// SubagentCall is how far the agents one call started have got: what a chat
// row shows of a running workflow. Revision moves with any write to them, and
// tells an open page to read again.
type SubagentCall struct {
	CallID   string
	Agents   int
	Done     int
	Revision int64
}

// ChildFingerprints is one chat's children's fingerprints by key, so a pass
// reparses only what changed.
func (s *Store) ChildFingerprints(parent session.Meta) (map[string]int64, error) {
	rows, err := s.db.Query(`SELECT c.key, c.fingerprint FROM subagents c JOIN sessions p ON p.sid = c.parent_sid
		WHERE p.agent = ? AND p.session_id = ?`, string(parent.Agent), parent.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var childKey string
		var fp int64
		if err := rows.Scan(&childKey, &fp); err != nil {
			return nil, err
		}
		out[childKey] = fp
	}
	return out, rows.Err()
}

// SyncSubagents writes one chat's children as listed now, with the transcript
// of each child in events, and drops those no longer listed; it returns the
// calls whose children changed. A chat not yet synced waits for a later pass.
func (s *Store) SyncSubagents(parent session.Meta, children []session.Child, events map[string][]agent.Event) ([]string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var sid int64
	switch err := tx.QueryRow(`SELECT sid FROM sessions WHERE agent = ? AND session_id = ?`,
		string(parent.Agent), parent.ID).Scan(&sid); err {
	case nil:
	case sql.ErrNoRows:
		return nil, nil
	default:
		return nil, err
	}
	stored, err := subagentRows(tx, `WHERE parent_sid = ?`, sid)
	if err != nil {
		return nil, err
	}
	byKey := map[string]Subagent{}
	for _, row := range stored {
		byKey[row.Key] = row
	}

	var changed []string
	mark := func(callID string) {
		if callID != "" && !slices.Contains(changed, callID) {
			changed = append(changed, callID)
		}
	}
	listed := map[string]bool{}
	for _, child := range children {
		listed[child.Key] = true
		before, known := byKey[child.Key]
		row := Subagent{
			Key: child.Key, CallID: child.CallID, Kind: child.Kind, Name: child.Name,
			RunID: child.RunID, Label: child.Label, Phase: child.Phase, Order: child.Order,
			Done: child.Done, Result: child.Result,
			StartedAt: before.StartedAt, UpdatedAt: child.UpdatedAt().UnixMilli(),
		}
		fresh, reread := events[child.Key]
		if reread {
			row.StartedAt = firstAt(fresh)
		}
		if known && row == before && !reread {
			continue
		}
		// A child whose transcript was not read keeps the fingerprint of the
		// one stored, or none, so the next pass reads it.
		var fingerprint int64
		if reread {
			fingerprint = child.Fingerprint()
		}
		var id int64
		if err := tx.QueryRow(`INSERT INTO subagents (parent_sid, key, call_id, kind, name, run_id, label, phase,
				run_order, done, result, started_at, updated_at, fingerprint, rev)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (parent_sid, key) DO UPDATE SET call_id = excluded.call_id, kind = excluded.kind,
				name = excluded.name, run_id = excluded.run_id, label = excluded.label, phase = excluded.phase,
				run_order = excluded.run_order, done = excluded.done, result = excluded.result,
				started_at = excluded.started_at, updated_at = excluded.updated_at,
				fingerprint = CASE WHEN ? THEN excluded.fingerprint ELSE fingerprint END, rev = excluded.rev
			RETURNING id`,
			sid, row.Key, row.CallID, row.Kind, row.Name, row.RunID, row.Label, row.Phase,
			row.Order, row.Done, row.Result, row.StartedAt, row.UpdatedAt, fingerprint, s.rev.Add(1), reread).Scan(&id); err != nil {
			return nil, err
		}
		if reread {
			if _, err := tx.Exec(`DELETE FROM subagent_events WHERE subagent = ?`, id); err != nil {
				return nil, err
			}
			if err := insertRows(tx, `INSERT INTO subagent_events (subagent, idx, kind, turn, text, tool, at) VALUES `, id, fresh); err != nil {
				return nil, err
			}
		}
		mark(row.CallID)
		mark(before.CallID)
	}
	for _, row := range stored {
		if listed[row.Key] {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM subagent_events WHERE subagent IN
			(SELECT id FROM subagents WHERE parent_sid = ? AND key = ?)`, sid, row.Key); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM subagents WHERE parent_sid = ? AND key = ?`, sid, row.Key); err != nil {
			return nil, err
		}
		// The call's other agents carry the news that it lost one.
		if _, err := tx.Exec(`UPDATE subagents SET rev = ? WHERE parent_sid = ? AND call_id = ?`,
			s.rev.Add(1), sid, row.CallID); err != nil {
			return nil, err
		}
		mark(row.CallID)
	}
	return changed, tx.Commit()
}

// Subagents is what one call of a chat started, in the order a workflow ran
// its agents.
func (s *Store) Subagents(chat ChatID, callID string) ([]Subagent, error) {
	_, sid, err := s.header(chat)
	if err != nil {
		return nil, err
	}
	return subagentRows(s.db, `WHERE parent_sid = ? AND call_id = ?`, sid, callID)
}

// SubagentMessages is one child's transcript: the child of callID named by
// childKey, or with no key the call's only one.
func (s *Store) SubagentMessages(chat ChatID, callID, childKey string) (Subagent, []Message, error) {
	_, sid, err := s.header(chat)
	if err != nil {
		return Subagent{}, nil, err
	}
	rows, err := subagentRows(s.db, `WHERE parent_sid = ? AND call_id = ? AND (? = '' OR key = ?)`,
		sid, callID, childKey, childKey)
	if err != nil {
		return Subagent{}, nil, err
	}
	if len(rows) == 0 {
		return Subagent{}, nil, fmt.Errorf("%w: no agent of call %q", ErrNotFound, callID)
	}
	events, err := s.events(`SELECT idx, kind, COALESCE(turn,''), COALESCE(text,''), COALESCE(tool,''), COALESCE(at,0)
		FROM subagent_events WHERE subagent = (SELECT id FROM subagents WHERE parent_sid = ? AND key = ?) ORDER BY idx`,
		sid, rows[0].Key)
	return rows[0], events, err
}

// SubagentCalls is how far each call of a chat that started agents has got.
func (s *Store) SubagentCalls(chat ChatID) ([]SubagentCall, error) {
	_, sid, err := s.header(chat)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT call_id, COUNT(*), SUM(done), MAX(rev) FROM subagents
		WHERE parent_sid = ? AND call_id != '' GROUP BY call_id ORDER BY MIN(id)`, sid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubagentCall{}
	for rows.Next() {
		var c SubagentCall
		if err := rows.Scan(&c.CallID, &c.Agents, &c.Done, &c.Revision); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func subagentRows(db querier, where string, args ...any) ([]Subagent, error) {
	rows, err := db.Query(`SELECT key, call_id, kind, name, run_id, label, phase, run_order, done, result,
		started_at, updated_at FROM subagents `+where+` ORDER BY run_order, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Subagent{}
	for rows.Next() {
		var r Subagent
		if err := rows.Scan(&r.Key, &r.CallID, &r.Kind, &r.Name, &r.RunID, &r.Label, &r.Phase, &r.Order,
			&r.Done, &r.Result, &r.StartedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// dropSubagents deletes one chat's children with it.
func dropSubagents(tx *sql.Tx, kind, sessionID string) error {
	const parent = `SELECT sid FROM sessions WHERE agent = ? AND session_id = ?`
	if _, err := tx.Exec(`DELETE FROM subagent_events WHERE subagent IN
		(SELECT id FROM subagents WHERE parent_sid IN (`+parent+`))`, kind, sessionID); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM subagents WHERE parent_sid IN (`+parent+`)`, kind, sessionID)
	return err
}

// firstAt is when a transcript began: its first line that carries a time.
func firstAt(events []agent.Event) int64 {
	for _, e := range events {
		if e.At != 0 {
			return e.At
		}
	}
	return 0
}
