package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/agent"
)

// tokenKeep is how long a thread's tools token is kept after its last use.
const tokenKeep = 30 * 24 * time.Hour

// toolTokens keeps the RepoGo tools token each thread on the background
// server opened with. The server keeps a thread loaded, with the MCP entry it
// opened with, after RepoGo's session closes and across host restarts, so a
// later session must answer to the same token. path "" keeps them in memory.
type toolTokens struct {
	path string

	mu     sync.Mutex
	loaded bool
	tokens map[string]tokenEntry
}

type tokenEntry struct {
	Token string    `json:"token"`
	Used  time.Time `json:"used"`
}

func (t *toolTokens) get(thread string) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loadLocked()
	return t.tokens[thread].Token
}

// put records the token thread opened with, and drops ones long unused.
func (t *toolTokens) put(thread, token string) {
	if thread == "" || token == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.loadLocked()
	now := time.Now()
	t.tokens[thread] = tokenEntry{Token: token, Used: now}
	for id, e := range t.tokens {
		if now.Sub(e.Used) > tokenKeep {
			delete(t.tokens, id)
		}
	}
	if t.path == "" {
		return
	}
	b, err := json.Marshal(t.tokens)
	if err != nil || os.MkdirAll(filepath.Dir(t.path), 0o700) != nil {
		return
	}
	tmp := t.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, t.path)
	}
}

func (t *toolTokens) loadLocked() {
	if t.loaded {
		return
	}
	t.loaded, t.tokens = true, map[string]tokenEntry{}
	if t.path == "" {
		return
	}
	if b, err := os.ReadFile(t.path); err == nil {
		_ = json.Unmarshal(b, &t.tokens)
	}
}

// bearer is the token an entry from Tools.Attach carries.
func bearer(entry agent.MCPServer) string {
	for _, h := range entry.Headers {
		if token, ok := strings.CutPrefix(h.Value, "Bearer "); ok && h.Name == "Authorization" {
			return token
		}
	}
	return ""
}

// ownedKeep is how long a turn's id is remembered after it starts: past a
// hook's drop being read, however long the turn ran.
const ownedKeep = 24 * time.Hour

// ownedTurns are the Codex turns RepoGo ran. On the shared server a hook
// can't carry REPOGO_ORIGIN, so the turn id it names is what says the turn
// was the app's and not the terminal's.
type ownedTurns struct {
	mu    sync.Mutex
	turns map[string]time.Time
}

func (o *ownedTurns) add(turnID string) {
	if turnID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.turns == nil {
		o.turns = map[string]time.Time{}
	}
	now := time.Now()
	o.turns[turnID] = now
	for id, at := range o.turns {
		if now.Sub(at) > ownedKeep {
			delete(o.turns, id)
		}
	}
}

func (o *ownedTurns) has(turnID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.turns[turnID]
	return ok
}

// ownsTurn reports a hook fired in a turn RepoGo runs: one it named, or the
// one a chat's session is starting, whose id Codex has not answered with
// yet. Codex runs one turn per thread at a time, so that one is RepoGo's.
func (r *runner) ownsTurn(threadID, turnID string) bool {
	if turnID != "" && r.owned.has(turnID) {
		return true
	}
	if threadID == "" {
		return false
	}
	key := agent.ChatID(agent.KindCodex, threadID)
	s, ok := r.pool.Get(key)
	if !ok {
		return false
	}
	defer r.pool.Release(key, s)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.shared && s.io != nil && (s.turnID == "" || s.turnID == turnID)
}
