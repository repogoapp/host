package claude

import (
	"strings"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/chatwire"
	"github.com/repogo/host/internal/toollabel"
)

// ToolRow is a Claude tool call on the chat lane (chatwire.Labeler): its
// label, a TodoWrite's list, a subagent's type, and whether a subagent was
// launched to run in the background.
func (p *Provider) ToolRow(call agent.ToolCall) chatwire.Tool {
	background := strings.HasPrefix(call.Output, "Async agent launched")
	if call.Name == "" {
		return chatwire.Tool{Background: background}
	}
	args, _ := toollabel.Input(call.Input)
	row := chatwire.Labeled(call.Name, label(call.Name, args), args)
	row.Background = background
	switch call.Name {
	case "TodoWrite":
		row.Todos = chatwire.Todos(call.Output, args)
	case "Task", "Agent":
		row.SubagentType = strings.TrimSpace(toollabel.String(args, "subagent_type"))
	}
	return row
}

// Resolve is the call as a sheet shows it: Claude's calls are what they say.
func (p *Provider) Resolve(call agent.ToolCall) agent.ToolCall { return call }

func label(name string, args map[string]any) toollabel.Label {
	switch name {
	case "Read":
		return toollabel.Read(args)
	case "Edit", "MultiEdit", "Write":
		return toollabel.Edit(args)
	case "Bash":
		return toollabel.Shell(args)
	case "LS":
		return toollabel.List(args)
	case "Grep":
		return toollabel.Grep(args)
	case "Glob":
		return toollabel.Glob(args)
	case "WebSearch":
		return toollabel.WebSearch(toollabel.String(args, "query"))
	case "WebFetch":
		return toollabel.WebFetch(args)
	case "Task", "Agent":
		return toollabel.Subagent(args)
	case "TaskCreate":
		subject := toollabel.Preview(toollabel.String(args, "subject"), 48, "task")
		return toollabel.New("checklist", "Creating task: "+subject, "Created task", "Task creation attempted")
	case "TaskUpdate":
		return taskUpdate(args)
	case "TodoWrite":
		return toollabel.Todos(args)
	case "AskUserQuestion":
		return toollabel.Question(args)
	case "ToolSearch":
		return toollabel.ToolSearch(args, func(name string) toollabel.Label { return label(name, nil) })
	case "EnterPlanMode":
		return toollabel.EnterPlanMode()
	case "ExitPlanMode":
		return toollabel.ExitPlanMode()
	case "Skill":
		skill := toollabel.Preview(toollabel.String(args, "skill"), 40, "skill")
		return toollabel.New("arrow.down.doc", "Loading skill: "+skill, "Loaded skill: "+skill, "Skill load attempted")
	case "Monitor":
		what := toollabel.Preview(toollabel.String(args, "description"), 40, "a background task")
		return toollabel.New("waveform.path.ecg", "Watching "+what, "Watched "+what, "Watching failed")
	case "TaskOutput":
		return toollabel.New("text.alignleft", "Reading background output", "Read background output", "Reading output failed")
	case "TaskStop":
		return toollabel.New("stop.circle", "Stopping a background task", "Stopped a background task", "Stopping failed")
	case "SendMessage":
		target := "another agent"
		if to := toollabel.String(args, "to"); to != "" {
			target = toollabel.Preview(to, 30, "")
		}
		return toollabel.New("arrow.turn.down.right", "Messaging "+target, "Messaged "+target, "Messaging failed")
	case "ListAgents":
		return toollabel.New("list.bullet", "Listing agents", "Listed agents", "Listing agents failed")
	case "ScheduleWakeup":
		return toollabel.New("alarm", "Scheduling a wake-up", "Scheduled a wake-up", "Scheduling failed")
	case "LSP":
		return toollabel.New("curlybraces", "Asking the language server", "Asked the language server", "Language server failed")
	case "PushNotification":
		return toollabel.New("bell", "Sending a notification", "Sent a notification", "Notification failed")
	}
	return toollabel.Shared(name, args)
}

// taskUpdate reads as the task's subject when the update names one, else its id.
func taskUpdate(args map[string]any) toollabel.Label {
	id := toollabel.String(args, "taskId")
	if id == "" {
		id = "task"
	}
	task := "#" + id
	if subject, ok := args["subject"].(string); ok {
		task = toollabel.Preview(subject, 42, id)
	}
	return toollabel.New("checklist", "Updating task "+task, "Updated task", "Task update attempted")
}
