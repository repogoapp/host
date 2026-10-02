package agents_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/session"
)

// The record shapes below are real ones, found by surveying every Claude and
// Codex transcript on a working machine (09-24): each was either shown as
// something it is not, or lost.

func parseClaude(t *testing.T, record map[string]any) []agent.Event {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return session.Parse(&claude.Sessions{}, raw)
}

func parseCodex(t *testing.T, payloadType string, payload map[string]any) []agent.Event {
	t.Helper()
	payload["type"] = payloadType
	raw, err := json.Marshal(map[string]any{"type": "response_item", "timestamp": "2026-09-24T10:00:00Z", "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	return session.Parse(&codex.Sessions{}, raw)
}

func TestClaudeDropsWhatTheCLIWroteForTheUser(t *testing.T) {
	for name, record := range map[string]map[string]any{
		"isMeta continuation": {"type": "user", "isMeta": true,
			"message": map[string]any{"content": []map[string]any{{"type": "text", "text": "Continue from where you left off."}}}},
		"isMeta hook feedback": {"type": "user", "isMeta": true,
			"message": map[string]any{"content": "Stop hook feedback: Background agents still running"}},
		"compact summary": {"type": "user", "isCompactSummary": true,
			"message": map[string]any{"content": "This session is being continued from a previous conversation…"}},
		"no response requested": {"type": "assistant",
			"message": map[string]any{"model": "<synthetic>", "content": []map[string]any{{"type": "text", "text": "No response requested."}}}},
	} {
		if got := parseClaude(t, record); len(got) != 0 {
			t.Errorf("%s: got %+v, want nothing", name, got)
		}
	}
}

func TestClaudeAPIErrorFailsTheTurn(t *testing.T) {
	got := parseClaude(t, map[string]any{"type": "assistant", "isApiErrorMessage": true,
		"message": map[string]any{"model": "<synthetic>", "content": []map[string]any{{"type": "text", "text": "API Error: 529 Overloaded."}}}})
	if len(got) != 1 || got[0].Kind != agent.EventTurnFailed || got[0].Error != "API Error: 529 Overloaded." {
		t.Fatalf("got %+v, want a failed turn carrying the error", got)
	}
}

// A prompt typed while a turn runs reaches the transcript as an attachment,
// and so does a background agent's notice.
func TestClaudeReadsQueuedCommands(t *testing.T) {
	got := parseClaude(t, map[string]any{"type": "attachment",
		"attachment": map[string]any{"type": "queued_command", "commandMode": "prompt", "prompt": "also check the widget"}})
	if len(got) != 1 || got[0].Kind != agent.EventUserMessage || got[0].Text != "also check the widget" {
		t.Fatalf("prompt: got %+v", got)
	}

	notice := "<task-notification>\n<tool-use-id>toolu_bg</tool-use-id>\n<status>completed</status>\n<result>Done.</result>\n</task-notification>"
	got = parseClaude(t, map[string]any{"type": "attachment",
		"attachment": map[string]any{"type": "queued_command", "commandMode": "task-notification", "prompt": notice}})
	if len(got) != 1 || got[0].Kind != agent.EventToolResult || got[0].Tool.CallID != "toolu_bg" || got[0].Tool.Output != "Done." {
		t.Fatalf("notice: got %+v", got)
	}

	if got := parseClaude(t, map[string]any{"type": "attachment",
		"attachment": map[string]any{"type": "queued_command", "isMeta": true, "prompt": "internal"}}); len(got) != 0 {
		t.Fatalf("meta: got %+v", got)
	}
	if got := parseClaude(t, map[string]any{"type": "attachment",
		"attachment": map[string]any{"type": "total_tokens_reminder"}}); len(got) != 0 {
		t.Fatalf("other attachment: got %+v", got)
	}
}

func TestClaudeLinksTheImagesAUserSent(t *testing.T) {
	t.Setenv("REPOGO_HOME", t.TempDir())
	got := parseClaude(t, map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{
		{"type": "image", "source": map[string]any{"type": "url", "url": "https://firebasestorage.googleapis.com/v0/b/x/o/a.jpg?alt=media"}},
		{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "aGVsbG8="}},
		{"type": "text", "text": "what is this"},
	}}})
	if len(got) != 1 || got[0].Kind != agent.EventUserMessage {
		t.Fatalf("got %+v", got)
	}
	lines := strings.Split(got[0].Text, "\n")
	if len(lines) != 3 || lines[0] != "what is this" ||
		lines[1] != "[@Image 1](https://firebasestorage.googleapis.com/v0/b/x/o/a.jpg?alt=media)" ||
		!strings.HasPrefix(lines[2], "[@Image 2](file://") {
		t.Fatalf("text = %q", got[0].Text)
	}
	path := strings.TrimSuffix(strings.TrimPrefix(lines[2], "[@Image 2](file://"), ")")
	if data, err := os.ReadFile(path); err != nil || string(data) != "hello" || filepath.Ext(path) != ".png" {
		t.Fatalf("cached image %s = %q, %v", path, data, err)
	}

	// Sent from the app, the image already has its file link.
	got = parseClaude(t, map[string]any{"type": "user", "message": map[string]any{"content": []map[string]any{
		{"type": "text", "text": "look [@a.png](file:///tmp/a.png)"},
		{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "aGVsbG8="}},
	}}})
	if len(got) != 1 || got[0].Text != "look [@a.png](file:///tmp/a.png)" {
		t.Fatalf("app send: got %+v", got)
	}
}

func TestCodexDropsTheStopNote(t *testing.T) {
	got := parseCodex(t, "message", map[string]any{"role": "user", "content": []map[string]any{
		{"type": "input_text", "text": "<turn_aborted>\nThe user interrupted the previous turn on purpose.\n</turn_aborted>"}}})
	if len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

func TestCodexPromptWithAnImageAndItsTags(t *testing.T) {
	t.Setenv("REPOGO_HOME", t.TempDir())
	got := parseCodex(t, "message", map[string]any{"role": "user", "content": []map[string]any{
		{"type": "input_text", "text": "<image name=[Image #1]>"},
		{"type": "input_image", "image_url": "data:image/jpeg;base64,aGVsbG8="},
		{"type": "input_text", "text": "</image>"},
		{"type": "input_text", "text": "Is this a codex error?"},
	}})
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	lines := strings.Split(got[0].Text, "\n")
	if len(lines) != 2 || lines[0] != "Is this a codex error?" || !strings.HasPrefix(lines[1], "[@Image 1](file://") ||
		!strings.HasSuffix(lines[1], ".jpg)") {
		t.Fatalf("text = %q", got[0].Text)
	}
}

// Sent from the app, an image arrives inline and as its file link; the link
// is the attachment, shown once.
func TestCodexAppSendKeepsOnlyTheFileLink(t *testing.T) {
	t.Setenv("REPOGO_HOME", t.TempDir())
	got := parseCodex(t, "message", map[string]any{"role": "user", "content": []map[string]any{
		{"type": "input_text", "text": "look [@a.png](file:///tmp/a.png)"},
		{"type": "input_image", "image_url": "data:image/png;base64,aGVsbG8="},
	}})
	if len(got) != 1 || got[0].Text != "look [@a.png](file:///tmp/a.png)" {
		t.Fatalf("got %+v", got)
	}
}

func TestCodexUnwrapsTheMobileAppsRequest(t *testing.T) {
	for _, heading := range []string{"## My request for Codex:", "## My request:"} {
		text := "# Files mentioned by the user:\n\n## Photo 1.jpg: /tmp/codex-remote-attachments/abc/Photo 1.jpg\n\n" +
			heading + "\nWhy does this crash?\n"
		got := parseCodex(t, "message", map[string]any{"role": "user", "content": []map[string]any{{"type": "input_text", "text": text}}})
		want := "Why does this crash?\n[@Photo 1.jpg](file:///tmp/codex-remote-attachments/abc/Photo 1.jpg)"
		if len(got) != 1 || got[0].Text != want {
			t.Fatalf("%s: got %+v, want %q", heading, got, want)
		}
	}
}

// A goal's turns open with Codex's own prompt; the objective is what the user
// typed, shown once when the goal is set and not again on pause or resume.
func TestCodexGoalObjectiveIsTheUserMessage(t *testing.T) {
	goalLine := func(status string, updatedAt int) []byte {
		return []byte(`{"timestamp":"2026-09-26T22:42:00Z","type":"event_msg","payload":{"type":"thread_goal_updated",` +
			`"goal":{"objective":"please see why GitHub action is failing","status":"` + status +
			`","createdAt":1790462520,"updatedAt":` + strconv.Itoa(updatedAt) + `}}}`)
	}
	got := session.Parse(&codex.Sessions{}, goalLine("active", 1790462520))
	if len(got) != 1 || got[0].Kind != agent.EventUserMessage || got[0].Text != "please see why GitHub action is failing" {
		t.Fatalf("set: got %+v", got)
	}
	for _, status := range []string{"paused", "active"} {
		if got := session.Parse(&codex.Sessions{}, goalLine(status, 1790463000)); len(got) != 0 {
			t.Fatalf("%s later: got %+v, want nothing", status, got)
		}
	}
	prompt := parseCodex(t, "message", map[string]any{"role": "user", "content": []map[string]any{{"type": "input_text",
		"text": "<codex_internal_context source=\"goal\">\nContinue working toward the active thread goal.\n<objective>\nx\n</objective>"}}})
	if len(prompt) != 0 {
		t.Fatalf("goal prompt: got %+v, want nothing", prompt)
	}
}

func TestCodexWebSearchIsAToolCall(t *testing.T) {
	got := parseCodex(t, "web_search_call", map[string]any{"status": "completed",
		"action": map[string]any{"type": "search", "query": "swiftui sheet detents"}})
	if len(got) != 2 || got[0].Kind != agent.EventToolCall || got[1].Kind != agent.EventToolResult {
		t.Fatalf("got %+v, want a call and its result", got)
	}
	if got[0].Tool.Name != "web_search" || got[0].Tool.CallID == "" || got[0].Tool.CallID != got[1].Tool.CallID ||
		!strings.Contains(string(got[0].Tool.Input), "swiftui sheet detents") {
		t.Fatalf("call = %+v", got[0].Tool)
	}
	got = parseCodex(t, "web_search_call", map[string]any{"status": "completed",
		"action": map[string]any{"type": "open_page", "url": "https://developer.apple.com"}})
	if len(got) != 2 || got[0].Tool.Name != "web_fetch" {
		t.Fatalf("open_page: got %+v", got)
	}
}
