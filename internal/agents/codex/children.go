package codex

import (
	"bufio"
	"cmp"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bytedance/sonic"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/session"
)

// Children lists the threads a Codex thread spawned, one child per thread
// however often it was resumed, each linked to the spawn_agent call whose
// result names its thread id (older Codex) or its task path (newer).
func (c *Sessions) Children(meta session.Meta) ([]session.Child, error) {
	rollouts := c.childRollouts(meta.ID)
	if len(rollouts) == 0 {
		return nil, nil
	}
	spawns := c.spawnCalls(meta)

	byThread := map[string][]childRollout{}
	var order []string
	for _, r := range rollouts {
		if byThread[r.ThreadID] == nil {
			order = append(order, r.ThreadID)
		}
		byThread[r.ThreadID] = append(byThread[r.ThreadID], r)
	}
	out := make([]session.Child, 0, len(order))
	for _, thread := range order {
		files := byThread[thread]
		// Filenames embed an ISO timestamp, so lexical order is the order a
		// resumed thread's rollouts were written in.
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		first := files[0]
		child := session.Child{
			Key:    thread,
			CallID: cmp.Or(spawns[thread], spawns[first.AgentPath]),
			Kind:   cmp.Or(first.Role, strings.TrimPrefix(first.AgentPath, "/root/")),
			Name:   first.Nickname,
		}
		for i, f := range files {
			file := session.ChildFile{Path: f.Path, Size: f.size, ModTime: f.modTime}
			if i == 0 {
				file.SkipLines = f.HistoryStart
			}
			child.Files = append(child.Files, file)
		}
		out = append(out, child)
	}
	return out, nil
}

// ChildEvents reads a spawned thread's rollouts in order, skipping a forked
// child's copy of its parent's history. One unreadable rollout fails it all,
// so the cache keeps what it had rather than storing part of the thread.
func (c *Sessions) ChildEvents(meta session.Meta, child session.Child) ([]agent.Event, error) {
	var out []agent.Event
	var seq uint64
	for _, f := range child.Files {
		events, _, err := session.ReadFile(f.Path, lineOffset(f.Path, f.SkipLines), c, meta.ID, seq)
		if err != nil {
			return nil, err
		}
		if len(events) > 0 {
			seq = events[len(events)-1].Seq
		}
		out = append(out, events...)
	}
	codexTurns(out)
	return out, nil
}

// childRollout is one rollout of a spawned thread, as List peeked it.
type childRollout struct {
	codexSubagent
	size    int64
	modTime time.Time
}

// recentScan is how stale the scan of today's and yesterday's rollouts may
// be: every chat and every watching device reads the one scan.
const recentScan = time.Second

// childRollouts is every rollout a thread spawned: those List last saw, and
// any written since in today's or yesterday's folder, where a running
// thread's new children land between List's sweeps.
func (c *Sessions) childRollouts(parent string) []childRollout {
	c.childMu.Lock()
	known := append([]childRollout(nil), c.children[parent]...)
	c.childMu.Unlock()

	seen := map[string]bool{}
	for _, r := range known {
		seen[r.Path] = true
	}
	for _, r := range c.recentChildren()[parent] {
		if !seen[r.Path] {
			known = append(known, r)
		}
	}
	// A known rollout may have grown since it was seen: its size and time are now.
	for i, r := range known {
		if info, err := os.Stat(r.Path); err == nil {
			known[i].size, known[i].modTime = info.Size(), info.ModTime()
		}
	}
	return known
}

// recentChildren is the spawned rollouts in today's and yesterday's folders by
// parent thread, scanned at most once per recentScan.
func (c *Sessions) recentChildren() map[string][]childRollout {
	c.recentMu.Lock()
	defer c.recentMu.Unlock()
	if time.Since(c.recentAt) < recentScan {
		return c.recent
	}
	byParent := map[string][]childRollout{}
	now := time.Now()
	for _, day := range []time.Time{now, now.AddDate(0, 0, -1)} {
		dir := filepath.Join(c.home, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"))
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			path := filepath.Join(dir, e.Name())
			if sub := c.peek(path, info).subagent; sub != nil {
				byParent[sub.ParentThread] = append(byParent[sub.ParentThread],
					childRollout{codexSubagent: *sub, size: info.Size(), modTime: info.ModTime()})
			}
		}
	}
	c.recent, c.recentAt = byParent, time.Now()
	return byParent
}

// spawnCalls maps what each spawn_agent result names, a thread id or a task
// path, to the call's id; another tool's result naming one is not a spawn.
// Read again only when the parent's files change.
func (c *Sessions) spawnCalls(meta session.Meta) map[string]string {
	stamp := spawnStamp{size: meta.SizeBytes, updated: meta.UpdatedAt}
	c.childMu.Lock()
	cached, ok := c.spawns[meta.ID]
	c.childMu.Unlock()
	if ok && cached.stamp == stamp {
		return cached.calls
	}

	calls := map[string]string{}
	events, _ := session.ReadAll(meta, c)
	spawned := map[string]bool{}
	for _, e := range events {
		if e.Kind == agent.EventToolCall && e.Tool != nil && e.Tool.Name == "spawn_agent" {
			spawned[e.Tool.CallID] = true
		}
	}
	for _, e := range events {
		if e.Kind != agent.EventToolResult || e.Tool == nil || !spawned[e.Tool.CallID] {
			continue
		}
		var result struct {
			AgentID  string `json:"agent_id"`
			TaskName string `json:"task_name"`
		}
		if sonic.UnmarshalString(e.Tool.Output, &result) != nil {
			continue
		}
		for _, ref := range []string{result.AgentID, result.TaskName} {
			if ref != "" {
				calls[ref] = e.Tool.CallID
			}
		}
	}
	c.childMu.Lock()
	if c.spawns == nil {
		c.spawns = map[string]spawnCache{}
	}
	c.spawns[meta.ID] = spawnCache{stamp: stamp, calls: calls}
	c.childMu.Unlock()
	return calls
}

type spawnStamp struct {
	size    int64
	updated time.Time
}

type spawnCache struct {
	stamp spawnStamp
	calls map[string]string
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
