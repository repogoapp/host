package claude

import (
	"context"
	"encoding/json"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/clitool"
)

// executable is the claude a one-shot runs, native install first: an old npm
// global on PATH (1.0.x) reads stream-json and prints nothing.
func executable() string { return clitool.PreferredExecutable(definition().Tool.Spec) }

// streamJSONControl sends control requests (id -> subtype) to a throwaway
// `claude -p` reading stream-json and answers each by id; nil marks one that
// failed. No prompt is sent, so no turn runs and nothing is billed.
func streamJSONControl(ctx context.Context, requests map[string]string) (map[string]json.RawMessage, bool) {
	lines := make([]string, 0, len(requests))
	for id, subtype := range requests {
		line, _ := json.Marshal(map[string]any{
			"type":       "control_request",
			"request_id": id,
			"request":    map[string]string{"subtype": subtype},
		})
		lines = append(lines, string(line))
	}
	// The stream carries hook and system events too; replies match by request id.
	return agent.Exchange(ctx, executable(), []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose"},
		lines, len(requests), func(raw []byte) (string, json.RawMessage, bool) {
			var envelope struct {
				Type     string `json:"type"`
				Response struct {
					Subtype   string          `json:"subtype"`
					RequestID string          `json:"request_id"`
					Response  json.RawMessage `json:"response"`
				} `json:"response"`
			}
			if json.Unmarshal(raw, &envelope) != nil || envelope.Type != "control_response" {
				return "", nil, false
			}
			if _, wanted := requests[envelope.Response.RequestID]; !wanted {
				return "", nil, false
			}
			if envelope.Response.Subtype != "success" {
				return envelope.Response.RequestID, nil, true
			}
			return envelope.Response.RequestID, envelope.Response.Response, true
		})
}
