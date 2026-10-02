package claude

import "github.com/repogo/host/internal/claudecode"

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
	case "task_notification":
		task.terminal = true
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
	}
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
