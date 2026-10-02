package codex

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strconv"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session/native"
	"github.com/repogo/host/internal/shipping"
)

// forkCopyGap separates a fork's copied history from its own requests: Codex
// writes the parent's lines in one burst, milliseconds apart, and the fork's
// first real token count lands only after a model call, seconds later.
const forkCopyGap = 1000

// codexState is what one rollout's usage depends on across lines: its session,
// the model the latest turn_context names, whether it opens with a parent's
// copied history, and the running total that tells a repeat from a request.
type codexState struct {
	// id is this rollout's own thread, which keys its requests; session is the
	// chat they count under, the parent's for a spawned subagent.
	id         string
	session    string
	sawMeta    bool
	model      string
	copying    bool
	copyAnchor int64
	lastTotal  int64
	last       [5]int64
}

// parse is one token_count line's usage, or nil for a line that repeats the
// last one, belongs to a fork's copied history, or carries none. A
// session_meta or turn_context line only updates the state.
func (c *codexState) parse(line []byte) *shipping.Record {
	var root struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		Payload   struct {
			ID           string `json:"id"`
			ForkedFromID string `json:"forked_from_id"`
			Source       any    `json:"source"`
			Model        string `json:"model"`
			Type         string `json:"type"`
			Info         *struct {
				Last *struct {
					Input      any `json:"input_tokens"`
					Cached     any `json:"cached_input_tokens"`
					CacheWrite any `json:"cache_write_input_tokens"`
					Output     any `json:"output_tokens"`
					Reasoning  any `json:"reasoning_output_tokens"`
				} `json:"last_token_usage"`
				Total *struct {
					Total any `json:"total_tokens"`
				} `json:"total_token_usage"`
			} `json:"info"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &root) != nil {
		return nil
	}
	ts := agent.UnixMillis(root.Timestamp)
	switch root.Type {
	case "session_meta":
		// Only the first names this file's session; a fork repeats its ancestors' after it.
		if c.sawMeta {
			return nil
		}
		c.sawMeta, c.id, c.session = true, root.Payload.ID, root.Payload.ID
		parent := spawnedBy(root.Payload.Source)
		if parent != "" {
			c.session = parent
		}
		if root.Payload.ForkedFromID != "" || parent != "" {
			c.copying, c.copyAnchor = true, ts
		}
		return nil
	case "turn_context":
		if root.Payload.Model != "" {
			c.model = root.Payload.Model
		}
		return nil
	}
	if root.Payload.Type != "token_count" || root.Payload.Info == nil || root.Payload.Info.Last == nil {
		return nil
	}
	if c.model == "" || ts == 0 {
		return nil
	}
	last := root.Payload.Info.Last
	sig := [5]int64{shipping.Positive(last.Input), shipping.Positive(last.Cached), shipping.Positive(last.CacheWrite), shipping.Positive(last.Output), shipping.Positive(last.Reasoning)}
	var total int64
	if root.Payload.Info.Total != nil {
		total = shipping.Positive(root.Payload.Info.Total.Total)
	}
	// Codex restates the previous count on some stream boundaries; the
	// running total says so, else the whole reading does.
	repeat := (total > 0 && total == c.lastTotal) || (total == 0 && sig == c.last)
	c.lastTotal, c.last = total, sig
	if c.copying {
		if ts-c.copyAnchor < forkCopyGap {
			c.copyAnchor = ts
			return nil
		}
		c.copying = false
	}
	if repeat {
		return nil
	}
	// Codex's input_tokens includes the cached and cache-write portions.
	uncached := max(0, max(0, sig[0]-sig[1])-sig[2])
	t := shipping.Tokens{Uncached: uncached, Cached: sig[1], Creation: sig[2], Output: sig[3], Reasoning: sig[4]}
	if t.Uncached+t.Cached+t.Creation+t.Output == 0 {
		return nil
	}
	rec := &shipping.Record{Session: c.session, TimestampMs: ts, Model: c.model, Tokens: t}
	// The running total is unique within a thread, and a resumed segment that
	// restates a request restates its total too. The thread's own id, since a
	// subagent's total starts where its parent's stood and would collide.
	if total > 0 && c.id != "" {
		rec.Key = c.id + ":" + strconv.FormatInt(total, 10)
	}
	return rec
}

// spawnedBy is the parent thread a session_meta source names, "" for none: a
// subagent's rollout opens with its parent's history, like a fork, and its
// usage is the parent chat's, as a Claude subagent's is.
func spawnedBy(source any) string {
	s, _ := source.(map[string]any)
	sub, _ := s["subagent"].(map[string]any)
	spawn, _ := sub["thread_spawn"].(map[string]any)
	parent, _ := spawn["parent_thread_id"].(string)
	return parent
}

// UsageRoots is where the usage ledger finds Codex's rollouts: live ones,
// subagents' included, and the ones the user archived.
func (p *Provider) UsageRoots() []string {
	return []string{filepath.Join(p.home, "sessions"), filepath.Join(p.home, "archived_sessions")}
}

// A rollout line matters to usage only if it carries one of these: a token
// count, the turn context that names the model, or the session's own meta.
var (
	tokenCountNeedle  = []byte(`"token_count"`)
	turnContextNeedle = []byte(`"turn_context"`)
	sessionMetaNeedle = []byte(`"session_meta"`)
)

// NewUsageParser reads one rollout's token usage. Each call starts fresh
// state, because the session, model and running total belong to one file.
func (p *Provider) NewUsageParser() func([]byte) *shipping.Record {
	state := &codexState{}
	return func(line []byte) *shipping.Record {
		if !bytes.Contains(line, tokenCountNeedle) && !bytes.Contains(line, turnContextNeedle) && !bytes.Contains(line, sessionMetaNeedle) {
			return nil
		}
		return state.parse(line)
	}
}

// ParseUsageFile reads a whole file's usage with the Rust parser when the
// host links it; the ledger falls back to NewUsageParser otherwise.
func (p *Provider) ParseUsageFile(path string) ([]shipping.Record, bool) {
	return shipping.ParseNative(native.ParserCodex, path)
}
