package claude

import (
	"slices"

	"github.com/repogo/host/internal/claudecode"
)

type liveTask struct {
	owner      string
	subagent   bool
	terminal   bool
	ambient    bool
	background bool
}

func (s *liveSession) updateTaskLocked(m claudecode.Message) {
	if m.Subtype == "background_tasks_changed" {
		live := map[string]bool{}
		for _, entry := range m.Tasks {
			live[entry.TaskID] = true
			if task := s.tasks[entry.TaskID]; task != nil {
				task.background = true
				task.ambient = entry.Ambient
			}
		}
		for id, task := range s.tasks {
			if task.background && !live[id] {
				task.terminal = true
			}
		}
		s.log.Debug("Claude background tasks changed", "running", s.runningTasksLocked())
		return
	}
	if m.TaskID == "" {
		return
	}
	task := s.tasks[m.TaskID]
	if task == nil {
		task = &liveTask{}
		s.tasks[m.TaskID] = task
	}
	switch m.Subtype {
	case "task_started":
		task.subagent = task.subagent || m.SubagentType != ""
		task.ambient = m.Ambient || m.SkipTranscript
		task.background = m.IsBackgrounded
		if s.io != nil && task.owner == "" {
			task.owner = s.io.TurnID
		}
		s.log.Debug("Claude task started", "task", m.TaskID, "type", m.TaskType, "subagent", m.SubagentType, "background", task.background, "ambient", task.ambient)
	case "task_notification":
		task.terminal = true
		s.log.Debug("Claude task finished", "task", m.TaskID, "status", m.Status, "running", s.runningTasksLocked())
	case "task_updated":
		if m.Patch.IsBackgrounded != nil {
			task.background = *m.Patch.IsBackgrounded
		}
		switch m.Patch.Status {
		case "completed", "failed", "killed":
			task.terminal = true
		case "running", "pending":
			task.terminal = false
			if s.io != nil {
				task.owner = s.io.TurnID
			}
		}
		s.log.Debug("Claude task updated", "task", m.TaskID, "status", m.Patch.Status, "background", task.background, "running", s.runningTasksLocked())
	}
}

// runningTasksLocked is the ids of the tasks that keep the process busy, as
// busyLocked counts them.
func (s *liveSession) runningTasksLocked() []string {
	var ids []string
	for id, task := range s.tasks {
		if !task.terminal && !task.ambient {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

func (s *liveSession) awaitingSubagentsLocked() bool {
	if s.io == nil {
		return false
	}
	for _, task := range s.tasks {
		if task.owner == s.io.TurnID && task.subagent && !task.ambient && !task.terminal {
			return true
		}
	}
	return false
}
