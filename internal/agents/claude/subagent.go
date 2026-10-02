package claude

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/bytedance/sonic"

	"github.com/repogo/host/internal/session"
)

// Subagent reads the agent a Task or Agent call started. It is found by the
// sidecar the CLI writes beside each subagent's transcript,
// subagents/agent-<id>.meta.json, which names the call that started it.
func (c *Sessions) Subagent(meta session.Meta, callID string) (session.Subagent, error) {
	dir := filepath.Join(strings.TrimSuffix(meta.Path, ".jsonl"), "subagents")
	sidecars, _ := filepath.Glob(filepath.Join(dir, "agent-*.meta.json"))
	for _, sidecar := range sidecars {
		data, err := os.ReadFile(sidecar)
		if err != nil {
			continue
		}
		var m struct {
			AgentType string `json:"agentType"`
			ToolUseID string `json:"toolUseId"`
		}
		if sonic.Unmarshal(data, &m) != nil || m.ToolUseID != callID {
			continue
		}
		path := strings.TrimSuffix(sidecar, ".meta.json") + ".jsonl"
		events, _, err := session.ReadFile(path, 0, &Sessions{Sidechain: true}, meta.ID, 0)
		if err != nil {
			return session.Subagent{}, err
		}
		events = c.Normalize(events)
		return session.Subagent{Kind: m.AgentType, Events: events}, nil
	}
	return session.Subagent{}, session.ErrNotFound
}
