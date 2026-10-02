package claude

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session/native"
	"github.com/repogo/host/internal/shipping"
)

// unpriced are model names that are not one billable model: the CLI's own
// replies, and aliases naming a family, which would price one tier at another's rate.
var unpriced = map[string]bool{
	"<synthetic>": true, "synthetic": true,
	"opus": true, "sonnet": true, "haiku": true, "fable": true,
}

// parseClaude is one assistant line's token usage, or nil for any other line.
// A `<synthetic>` reply is the CLI's own text, not a request, so it bills nothing.
func parseClaude(line []byte) *shipping.Record {
	var root struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
		SessionID string `json:"sessionId"`
		RequestID string `json:"requestId"`
		CostUSD   any    `json:"costUSD"`
		Message   struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				Input         any    `json:"input_tokens"`
				Cached        any    `json:"cache_read_input_tokens"`
				Creation      any    `json:"cache_creation_input_tokens"`
				Output        any    `json:"output_tokens"`
				Speed         string `json:"speed"`
				CacheCreation *struct {
					FiveMinutes any `json:"ephemeral_5m_input_tokens"`
					OneHour     any `json:"ephemeral_1h_input_tokens"`
				} `json:"cache_creation"`
			} `json:"usage"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &root) != nil || root.Type != "assistant" || root.Message.Usage == nil {
		return nil
	}
	ts := agent.UnixMillis(root.Timestamp)
	if ts == 0 || root.Message.Model == "" || root.Message.Model == "<synthetic>" {
		return nil
	}
	model := root.Message.Model
	if unpriced[strings.ToLower(model)] {
		model = ""
	}
	usage := root.Message.Usage
	tokens := shipping.Tokens{
		Uncached: shipping.Positive(usage.Input),
		Cached:   shipping.Positive(usage.Cached),
		Creation: shipping.Positive(usage.Creation),
		Output:   shipping.Positive(usage.Output),
	}
	// The split by cache lifetime prices differently; without it every write is the 5-minute kind.
	if c := usage.CacheCreation; c != nil {
		five, hour := shipping.Positive(c.FiveMinutes), shipping.Positive(c.OneHour)
		if five+hour > 0 {
			tokens.Creation, tokens.Creation1h = five, hour
		}
	}
	rec := &shipping.Record{
		Session:     root.SessionID,
		TimestampMs: ts,
		Model:       model,
		Tokens:      tokens,
		Fast:        usage.Speed == "fast",
	}
	if cost, ok := root.CostUSD.(float64); ok && cost >= 0 {
		rec.ReportedUSD, rec.HasReported = cost, true
	}
	// Claude writes one response into several lines, and resumed and forked
	// sessions copy them into new files, all under the API's own ids.
	if root.Message.ID != "" || root.RequestID != "" {
		rec.Key = root.Message.ID + ":" + root.RequestID
	}
	return rec
}

// UsageRoots is where the usage ledger finds Claude's transcripts, subagents'
// included: they bill tokens though the chat list leaves them out.
func (p *Provider) UsageRoots() []string { return []string{filepath.Join(p.home, "projects")} }

// usageNeedle is in every line that carries token usage.
var usageNeedle = []byte(`"usage"`)

// NewUsageParser reads one transcript's token usage. A cheap byte check skips
// the lines that carry none before any JSON is decoded.
func (p *Provider) NewUsageParser() func([]byte) *shipping.Record {
	return func(line []byte) *shipping.Record {
		if !bytes.Contains(line, usageNeedle) {
			return nil
		}
		return parseClaude(line)
	}
}

// ParseUsageFile reads a whole file's usage with the Rust parser when the
// host links it; the ledger falls back to NewUsageParser otherwise.
func (p *Provider) ParseUsageFile(path string) ([]shipping.Record, bool) {
	return shipping.ParseNative(native.ParserClaude, path)
}
