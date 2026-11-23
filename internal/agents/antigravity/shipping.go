package antigravity

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/shipping"
)

// agy keeps each conversation under <home>/brain/<id>: its log is
// .system_generated/logs/transcript_full.jsonl, one line per step, and a
// model call's line names the model and its tokens. transcript.jsonl and
// logs/chunks/ beside it repeat the same steps, so only the full log counts.
const usageLog = "transcript_full.jsonl"

// UsageRoots is where the usage ledger finds agy's conversations, the
// terminal's and RepoGo's alike.
func (p *Provider) UsageRoots() []string { return []string{filepath.Join(p.home, "brain")} }

// NewUsageParser is unused: the ledger asks NewFileUsageParser, which knows
// the conversation by its folder.
func (p *Provider) NewUsageParser() func([]byte) *shipping.Record {
	return func([]byte) *shipping.Record { return nil }
}

// NewFileUsageParser reads one conversation's log; any other file under the
// root bills nothing.
func (p *Provider) NewFileUsageParser(path string) func([]byte) *shipping.Record {
	if filepath.Base(path) != usageLog {
		return func([]byte) *shipping.Record { return nil }
	}
	session := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(path))))
	cwd := p.conversations()[session].cwd
	return func(line []byte) *shipping.Record {
		if !bytes.Contains(line, modelCall) {
			return nil
		}
		return parseStep(line, session, cwd)
	}
}

// modelCall is in every line that is a model call.
var modelCall = []byte(`"PLANNER_RESPONSE"`)

// effortSuffix is the thinking agy writes after a model's id
// (gemini-4-neon-high, claude-opus-4-6-thinking), which no price list names.
var effortSuffix = regexp.MustCompile(`-(low|medium|high|xhigh|thinking)$`)

// parseStep is one model call's tokens. agy counts cached input apart from
// the rest, and its output includes the thinking.
func parseStep(line []byte, session, cwd string) *shipping.Record {
	var step struct {
		Index     *int64 `json:"step_index"`
		Type      string `json:"type"`
		CreatedAt string `json:"created_at"`
		Model     string `json:"model"`
		Input     any    `json:"input_tokens"`
		CacheRead any    `json:"cache_read_tokens"`
		Output    any    `json:"output_tokens"`
	}
	if json.Unmarshal(line, &step) != nil || step.Type != "PLANNER_RESPONSE" || step.Model == "" {
		return nil
	}
	ts := agent.UnixMillis(step.CreatedAt)
	tokens := shipping.Tokens{
		Uncached: shipping.Positive(step.Input),
		Cached:   shipping.Positive(step.CacheRead),
		Output:   shipping.Positive(step.Output),
	}
	if ts == 0 || tokens == (shipping.Tokens{}) {
		return nil
	}
	rec := &shipping.Record{
		Session: session, Cwd: cwd, TimestampMs: ts, Tokens: tokens,
		Model: effortSuffix.ReplaceAllString(step.Model, ""),
	}
	if step.Index != nil {
		rec.Key = session + ":" + strconv.FormatInt(*step.Index, 10)
	}
	return rec
}
