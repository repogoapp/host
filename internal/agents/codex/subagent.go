package codex

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bytedance/sonic"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session"
)

// Subagent finds the thread a spawn_agent call started, by the `agent_id`
// (older Codex) or `task_name` (newer) its result names, among the rollouts
// that name this session as their parent.
func (c *Sessions) Subagent(meta session.Meta, callID string) (session.Subagent, error) {
	events, err := session.ReadAll(meta, c)
	if err != nil {
		return session.Subagent{}, err
	}
	var spawned struct {
		AgentID  string `json:"agent_id"`
		TaskName string `json:"task_name"`
	}
	found := false
	for _, e := range events {
		if e.Kind == agent.EventToolResult && e.Tool != nil && e.Tool.CallID == callID {
			found = sonic.UnmarshalString(e.Tool.Output, &spawned) == nil
		}
	}
	if !found || (spawned.AgentID == "" && spawned.TaskName == "") {
		return session.Subagent{}, session.ErrNotFound
	}

	var files []codexSubagent
	for _, child := range c.subagents(meta.ID) {
		if (spawned.AgentID != "" && child.ThreadID == spawned.AgentID) ||
			(spawned.TaskName != "" && child.AgentPath == spawned.TaskName) {
			files = append(files, child)
		}
	}
	if len(files) == 0 {
		return session.Subagent{}, session.ErrNotFound
	}
	// A resumed child writes another rollout; filenames sort by time.
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	var out []agent.Event
	var seq uint64
	for i, f := range files {
		var offset int64
		if i == 0 {
			// A forked child's first lines are its parent's history, copied.
			offset = lineOffset(f.Path, f.HistoryStart)
		}
		evs, _, err := session.ReadFile(f.Path, offset, c, meta.ID, seq)
		if err != nil {
			continue
		}
		if len(evs) > 0 {
			seq = evs[len(evs)-1].Seq
		}
		out = append(out, evs...)
	}
	codexTurns(out)
	first := files[0]
	kind := first.Role
	if kind == "" {
		kind = strings.TrimPrefix(first.AgentPath, "/root/")
	}
	return session.Subagent{Kind: kind, Name: first.Nickname, Events: out}, nil
}

// subagents is every rollout a subagent of `parent` wrote. Walks the rollouts
// like List does; peeks are cached, so a repeat costs a stat per file.
func (c *Sessions) subagents(parent string) []codexSubagent {
	var out []codexSubagent
	_ = filepath.WalkDir(filepath.Join(c.home, "sessions"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if sub := c.peek(path, info).subagent; sub != nil && sub.ParentThread == parent {
			out = append(out, *sub)
		}
		return nil
	})
	return out
}

// lineOffset is the byte offset of line n (0-based) of the file, or 0 when the
// file is shorter or unreadable.
func lineOffset(path string, n int) int64 {
	if n <= 0 {
		return 0
	}
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	var offset int64
	for line := 0; line < n; line++ {
		chunk, err := r.ReadSlice('\n')
		offset += int64(len(chunk))
		for err == bufio.ErrBufferFull {
			chunk, err = r.ReadSlice('\n')
			offset += int64(len(chunk))
		}
		if err != nil {
			return 0
		}
	}
	return offset
}
