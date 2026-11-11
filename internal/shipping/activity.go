package shipping

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// activitySchema is time spent on projects, one table for every kind since
// they are always read on one timeline (HOST-MIGRATIONS.md).
const activitySchema = `CREATE TABLE IF NOT EXISTS activity (
  -- user: a device's screen time, uploaded by it (usage.record_time).
  -- prompt: the user typing, dictating or talking to the voice agent, uploaded
  -- the same way; screen time less prompting is reviewing.
  -- agent: an agent working on a turn, recorded from the chat's status;
  -- gaps between one turn's spans are its waits on the user.
  id         TEXT PRIMARY KEY,   -- user, prompt: the device's span ID; agent: the host's
  kind       TEXT NOT NULL,      -- 'user' | 'prompt' | 'agent'
  input      TEXT,               -- prompt only: 'typing' | 'dictation' | 'voice'
  cwd        TEXT NOT NULL,      -- the project folder
  actor      TEXT NOT NULL,      -- user, prompt: the device ID; agent: claude, codex…
  label      TEXT NOT NULL,      -- user, prompt: the device's name at upload; agent: the agent
  platform   TEXT,               -- user, prompt
  chat_id    TEXT,               -- agent only
  turn_id    TEXT,               -- agent only
  started_at INTEGER NOT NULL,   -- Unix ms
  ended_at   INTEGER,            -- a device's: its last checkpoint; agent: NULL while working
  updated_at INTEGER NOT NULL,
  CHECK (kind IN ('user', 'prompt', 'agent')),
  CHECK ((kind = 'prompt') = (input IS NOT NULL)),
  CHECK (input IS NULL OR input IN ('typing', 'dictation', 'voice')),
  CHECK (kind != 'agent' OR (chat_id IS NOT NULL AND turn_id IS NOT NULL)),
  CHECK (kind = 'agent' OR ended_at IS NOT NULL)
) WITHOUT ROWID`

// The kinds of activity.
const (
	ActivityUser   = "user"
	ActivityPrompt = "prompt"
	ActivityAgent  = "agent"
)

// What a prompt span was: typing in the composer, dictating, or talking to the voice agent.
var promptInputs = map[string]bool{"typing": true, "dictation": true, "voice": true}

// Limits on what a device may record.
const (
	maxSpan      = 24 * time.Hour
	maxClockSkew = 5 * time.Minute
	maxSpanBatch = 500
)

// Span is one stretch of activity on a project folder, as usage.time sends it.
type Span struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Input    string `json:"input,omitempty"`
	Cwd      string `json:"cwd"`
	Actor    string `json:"actor"`
	Label    string `json:"label"`
	Platform string `json:"platform,omitempty"`
	ChatID   string `json:"chat_id,omitempty"`
	TurnID   string `json:"turn_id,omitempty"`
	// Unix ms; an agent span still working ends at the time of the read.
	StartedAtMs int64 `json:"started_at_ms"`
	EndedAtMs   int64 `json:"ended_at_ms"`
}

// Device is who recorded a user span, from the pairing, never from the caller's params.
type Device struct {
	ID, Label, Platform string
}

// UserSpan is one stretch a device uploads: its own ID, so sending it again
// changes nothing but the end. Kind is "user" (screen time) or "prompt",
// which names its Input.
type UserSpan struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"`
	Input       string `json:"input,omitempty"`
	Cwd         string `json:"cwd"`
	StartedAtMs int64  `json:"started_at_ms"`
	EndedAtMs   int64  `json:"ended_at_ms"`
}

// RecordUser stores a device's spans: one already stored only moves its end
// forward, and only for its own device. One that can't be right (validSpan) is
// skipped, never failing the rest; skipped counts them.
func (l *Ledger) RecordUser(ctx context.Context, d Device, spans []UserSpan, now time.Time) (skipped int, err error) {
	if len(spans) > maxSpanBatch {
		return 0, fmt.Errorf("%w: at most %d spans a call", ErrRange, maxSpanBatch)
	}
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	insert, err := tx.PrepareContext(ctx, `INSERT INTO activity
		(id, kind, input, cwd, actor, label, platform, started_at, ended_at, updated_at)
		VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
		  ended_at = MAX(activity.ended_at, excluded.ended_at),
		  label = excluded.label, updated_at = excluded.updated_at
		WHERE activity.kind = excluded.kind AND activity.actor = excluded.actor`)
	if err != nil {
		return 0, err
	}
	defer insert.Close()
	for _, s := range spans {
		if !validSpan(s, now) {
			skipped++
			continue
		}
		if _, err := insert.ExecContext(ctx, s.ID, s.Kind, s.Input, filepath.Clean(s.Cwd), d.ID, d.Label, d.Platform,
			s.StartedAtMs, s.EndedAtMs, now.UnixMilli()); err != nil {
			return 0, err
		}
	}
	return skipped, tx.Commit()
}

// validSpan has an ID, a device's kind with the input it needs, an absolute
// folder, an end at or after its start, a day at most, and a start no further
// ahead than clock skew allows.
func validSpan(s UserSpan, now time.Time) bool {
	switch s.Kind {
	case ActivityUser:
		if s.Input != "" {
			return false
		}
	case ActivityPrompt:
		if !promptInputs[s.Input] {
			return false
		}
	default:
		return false
	}
	return s.ID != "" && len(s.ID) <= 64 && filepath.IsAbs(s.Cwd) &&
		s.EndedAtMs >= s.StartedAtMs && s.EndedAtMs-s.StartedAtMs <= maxSpan.Milliseconds() &&
		s.StartedAtMs <= now.Add(maxClockSkew).UnixMilli()
}

// Activity is every span in scope that overlaps [sinceMs, untilMs), every
// kind, oldest first. A span still open ends at now.
func (l *Ledger) Activity(ctx context.Context, sinceMs, untilMs int64, scope Scope, now time.Time) ([]Span, error) {
	if sinceMs >= untilMs {
		return nil, ErrRange
	}
	where, args := inFolders(scope)
	at := now.UnixMilli()
	rows, err := l.db.QueryContext(ctx, `SELECT id, kind, COALESCE(input, ''), cwd, actor, label, COALESCE(platform, ''),
		  COALESCE(chat_id, ''), COALESCE(turn_id, ''), started_at, COALESCE(ended_at, ?)
		FROM activity
		WHERE started_at < ? AND COALESCE(ended_at, ?) > ? AND `+where+`
		ORDER BY started_at`, append([]any{at, untilMs, at, sinceMs}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Span{}
	for rows.Next() {
		var s Span
		if err := rows.Scan(&s.ID, &s.Kind, &s.Input, &s.Cwd, &s.Actor, &s.Label, &s.Platform,
			&s.ChatID, &s.TurnID, &s.StartedAtMs, &s.EndedAtMs); err != nil {
			return nil, err
		}
		s.EndedAtMs = max(s.EndedAtMs, s.StartedAtMs)
		out = append(out, s)
	}
	return out, rows.Err()
}

// AgentStatus is a chat's status change as the store recorded it.
type AgentStatus struct {
	ChatID string
	// The agent, the half of the chat ID before the colon.
	Agent string
	Cwd   string
	// The turn it belongs to; working on a new one starts a new span.
	TurnID  string
	Working bool
	// When the status took effect; a span closes here.
	At time.Time
}

// AgentStatus runs the agent clock: working opens a span for the turn, and
// anything else (waiting on the user, done, stopped, failed) closes the open
// one. The same status twice changes nothing.
func (l *Ledger) AgentStatus(ctx context.Context, s AgentStatus) error {
	tx, err := l.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var openID, openTurn string
	err = tx.QueryRowContext(ctx, `SELECT id, turn_id FROM activity
		WHERE kind = 'agent' AND chat_id = ? AND ended_at IS NULL`, s.ChatID).Scan(&openID, &openTurn)
	open := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	at := s.At.UnixMilli()
	if open && (!s.Working || openTurn != s.TurnID) {
		if _, err := tx.ExecContext(ctx, `UPDATE activity SET ended_at = MAX(started_at, ?), updated_at = ?
			WHERE id = ?`, at, at, openID); err != nil {
			return err
		}
		open = false
	}
	if s.Working && !open && s.Cwd != "" && s.TurnID != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO activity
			(id, kind, cwd, actor, label, chat_id, turn_id, started_at, updated_at)
			VALUES (?, 'agent', ?, ?, ?, ?, ?, ?, ?)`,
			newSpanID(), filepath.Clean(s.Cwd), s.Agent, s.Agent, s.ChatID, s.TurnID, at, at); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// newSpanID is a random ID for a span the host opens.
func newSpanID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// CloseOpenAgents ends every agent span a stopped host left open, at its chat's
// last sign of life, so a crash never counts as work. Run once at start.
func (l *Ledger) CloseOpenAgents(ctx context.Context, lastSeen func(chatID string) (time.Time, bool)) (int, error) {
	rows, err := l.db.QueryContext(ctx, `SELECT id, chat_id, started_at FROM activity
		WHERE kind = 'agent' AND ended_at IS NULL`)
	if err != nil {
		return 0, err
	}
	type open struct {
		id, chat string
		started  int64
	}
	var spans []open
	for rows.Next() {
		var o open
		if err := rows.Scan(&o.id, &o.chat, &o.started); err != nil {
			rows.Close()
			return 0, err
		}
		spans = append(spans, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, o := range spans {
		end := o.started
		if at, ok := lastSeen(o.chat); ok {
			end = max(end, at.UnixMilli())
		}
		if _, err := l.db.ExecContext(ctx, `UPDATE activity SET ended_at = ?, updated_at = ? WHERE id = ?`,
			end, end, o.id); err != nil {
			return 0, err
		}
	}
	return len(spans), nil
}
