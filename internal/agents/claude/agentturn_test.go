package claude

import (
	"testing"

	"github.com/repogoapp/host/internal/agent"
)

// A task notification Claude wrote for itself marks where its own turn
// begins, ahead of the report it carries; a prompt the person typed does not.
func TestATaskNotificationMarksAnAgentTurn(t *testing.T) {
	c := &Sessions{}
	note := `{"type":"user","promptId":"p2","origin":{"kind":"task-notification"},` +
		`"message":{"role":"user","content":"<task-notification><tool-use-id>call1</tool-use-id>` +
		`<status>completed</status><result>done</result></task-notification>"}}`
	events, _ := c.ParseLine([]byte(note))
	if len(events) != 2 {
		t.Fatalf("events = %+v, want the marker and the result", events)
	}
	if events[0].Kind != agent.EventAgentTurn || events[0].Text != "task-notification" || events[0].TurnID != "p2" {
		t.Fatalf("first = %+v, want agent_turn from task-notification in turn p2", events[0])
	}
	if events[1].Kind != agent.EventToolResult || events[1].Tool == nil || events[1].Tool.CallID != "call1" {
		t.Fatalf("second = %+v, want the call's late result", events[1])
	}

	prompt := `{"type":"user","promptId":"p1","message":{"role":"user","content":"Again"}}`
	events, _ = c.ParseLine([]byte(prompt))
	if len(events) != 1 || events[0].Kind != agent.EventUserMessage {
		t.Fatalf("prompt = %+v, want one user_message", events)
	}

	// A background shell command's notice carries no report, but still wakes Claude.
	shell := `{"type":"user","promptId":"p3","origin":{"kind":"task-notification"},` +
		`"message":{"role":"user","content":"<task-notification><status>completed</status></task-notification>"}}`
	events, _ = c.ParseLine([]byte(shell))
	if len(events) != 1 || events[0].Kind != agent.EventAgentTurn {
		t.Fatalf("shell notice = %+v, want only the marker", events)
	}
}
