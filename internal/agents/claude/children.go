package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bytedance/sonic"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
	"github.com/repogoapp/host/internal/toollabel"
)

// Children lists the agents a Claude session started, from what the CLI
// writes beside its transcript: a sidecar naming each subagent's call, and a
// journal per Workflow run listing its agents as they start and end.
func (c *Sessions) Children(meta session.Meta) ([]session.Child, error) {
	dir := filepath.Join(strings.TrimSuffix(meta.Path, ".jsonl"), "subagents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	var out []session.Child
	for _, e := range entries {
		id, ok := strings.CutPrefix(e.Name(), "agent-")
		if id, ok = strings.CutSuffix(id, ".meta.json"); ok {
			if child, ok := subagentChild(dir, id); ok {
				out = append(out, child)
			}
		}
	}
	runs, _ := os.ReadDir(filepath.Join(dir, "workflows"))
	if len(runs) == 0 {
		return out, nil
	}
	calls := c.workflowCalls(meta)
	for _, run := range runs {
		if run.IsDir() {
			out = append(out, workflowChildren(filepath.Join(dir, "workflows", run.Name()), run.Name(), calls[run.Name()])...)
		}
	}
	return out, nil
}

// ChildEvents reads a child's own transcript, where every line is a sidechain.
func (c *Sessions) ChildEvents(meta session.Meta, child session.Child) ([]agent.Event, error) {
	if len(child.Files) == 0 {
		return nil, nil
	}
	events, _, err := session.ReadFile(child.Files[0].Path, 0, &Sessions{Sidechain: true}, meta.ID, 0)
	if err != nil {
		return nil, err
	}
	return c.Normalize(events), nil
}

// subagentChild is the subagent whose sidecar is agent-<id>.meta.json. One
// with no call (a compaction) is no call's work and is left out.
func subagentChild(dir, id string) (session.Child, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "agent-"+id+".meta.json"))
	if err != nil {
		return session.Child{}, false
	}
	var m struct {
		AgentType string `json:"agentType"`
		ToolUseID string `json:"toolUseId"`
	}
	if sonic.Unmarshal(data, &m) != nil || m.ToolUseID == "" {
		return session.Child{}, false
	}
	return session.Child{
		Key: id, CallID: m.ToolUseID, Kind: m.AgentType,
		Files: childFiles(filepath.Join(dir, "agent-"+id+".jsonl")),
	}, true
}

// workflowChildren is a run's agents from its journal, in the order they
// started.
func workflowChildren(dir, runID, callID string) []session.Child {
	data, err := os.ReadFile(filepath.Join(dir, "journal.jsonl"))
	if err != nil {
		return nil
	}
	var out []session.Child
	index := map[string]int{}
	for line := range bytes.SplitSeq(data, []byte("\n")) {
		var entry struct {
			Type    string          `json:"type"`
			AgentID string          `json:"agentId"`
			Label   string          `json:"label"`
			Phase   string          `json:"phase"`
			Result  json.RawMessage `json:"result"`
		}
		if len(line) == 0 || sonic.Unmarshal(line, &entry) != nil || entry.AgentID == "" ||
			strings.ContainsAny(entry.AgentID, `/\.`) {
			continue
		}
		switch entry.Type {
		case "started":
			if _, seen := index[entry.AgentID]; seen {
				continue
			}
			index[entry.AgentID] = len(out)
			out = append(out, session.Child{
				Key: entry.AgentID, CallID: callID, RunID: runID,
				Label: entry.Label, Phase: entry.Phase, Order: len(out),
				Files: childFiles(filepath.Join(dir, "agent-"+entry.AgentID+".jsonl")),
			})
		case "result":
			if i, ok := index[entry.AgentID]; ok {
				out[i].Done = true
				out[i].Result = toollabel.Preview(resultText(entry.Result), 160, "")
			}
		}
	}
	return out
}

// childFiles is the transcript at path as it stands, or none before the
// agent writes it.
func childFiles(path string) []session.ChildFile {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return []session.ChildFile{{Path: path, Size: info.Size(), ModTime: info.ModTime()}}
}

// workflowCalls maps each Workflow run of the session to the call that
// launched it, from the launch result's toolUseResult.runId: the journal
// does not name the call. Read again only when the transcript changes.
func (c *Sessions) workflowCalls(meta session.Meta) map[string]string {
	stamp := runStamp{size: meta.SizeBytes, updated: meta.UpdatedAt}
	c.runMu.Lock()
	cached, ok := c.runs[meta.Path]
	c.runMu.Unlock()
	if ok && cached.stamp == stamp {
		return cached.calls
	}
	calls := launchedRuns(meta.Path)
	c.runMu.Lock()
	if c.runs == nil {
		c.runs = map[string]runCache{}
	}
	c.runs[meta.Path] = runCache{stamp: stamp, calls: calls}
	c.runMu.Unlock()
	return calls
}

type runStamp struct {
	size    int64
	updated time.Time
}

type runCache struct {
	stamp runStamp
	calls map[string]string
}

// launchedRuns reads every Workflow launch result in a transcript: the
// tool_result block's call id, and the run id beside it. Only lines that
// mention a runId are decoded.
func launchedRuns(path string) map[string]string {
	calls := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return calls
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := reader.ReadBytes('\n')
		if bytes.Contains(line, []byte(`"runId"`)) {
			var l struct {
				ToolUseResult struct {
					RunID string `json:"runId"`
				} `json:"toolUseResult"`
				Message struct {
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			var blocks []struct {
				Type      string `json:"type"`
				ToolUseID string `json:"tool_use_id"`
			}
			if sonic.Unmarshal(line, &l) == nil && l.ToolUseResult.RunID != "" &&
				sonic.Unmarshal(l.Message.Content, &blocks) == nil {
				for _, b := range blocks {
					if b.Type == "tool_result" && b.ToolUseID != "" {
						calls[l.ToolUseResult.RunID] = b.ToolUseID
					}
				}
			}
		}
		if err != nil {
			return calls
		}
	}
}

// resultText is an agent's result as text: its answer, or the object a
// schema asked for as JSON.
func resultText(raw json.RawMessage) string {
	var text string
	if sonic.Unmarshal(raw, &text) == nil {
		return text
	}
	if string(raw) == "null" {
		return ""
	}
	return string(raw)
}
