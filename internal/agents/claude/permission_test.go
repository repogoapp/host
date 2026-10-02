package claude

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/claudecode"
)

func TestPermissionChoicesAndEffects(t *testing.T) {
	cases := []struct{ name, tool, input, suggestion, want string }{
		{"shell", "Bash", `{"command":"git status"}`, `{"type":"addRules","destination":"localSettings","behavior":"allow","rules":[{"toolName":"Bash","ruleContent":"git:*"}]}`, "Yes, and don't ask again for git commands"},
		{"edits", "Write", `{"file_path":"/project/file"}`, `{"type":"setMode","destination":"session","mode":"acceptEdits"}`, "Yes, allow all edits during this session"},
		{"read", "Read", `{"file_path":"/project/file"}`, `{"type":"addRules","destination":"session","behavior":"allow","rules":[{"toolName":"Read"}]}`, "Yes, during this session"},
		{"web", "WebFetch", `{"url":"https://example.com/path"}`, "", "Yes, and don't ask again for example.com"},
		{"mcp", "mcp__example__read", `{}`, `{"type":"addRules","destination":"session","behavior":"allow","rules":[{"toolName":"mcp__example__read"}]}`, "Yes, and don't ask again for mcp__example__read commands"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := claudecode.PermissionRequest{ToolName: tc.tool, Input: json.RawMessage(tc.input)}
			if tc.suggestion != "" {
				p.Suggestions = []json.RawMessage{json.RawMessage(tc.suggestion)}
			}
			options, updates := permissionOptions(p, "/project")
			if len(options) != 3 || options[1].Name != tc.want || len(updates["allow-with-updates"]) != 1 {
				t.Fatalf("options %#v, updates %#v", options, updates)
			}
			p.SuppressAlwaysAllow = true
			p.DefaultToNo = true
			options, _ = permissionOptions(p, "/project")
			if len(options) != 2 || options[0].OptionID != "reject" {
				t.Fatal(options)
			}
		})
	}
}

func TestPermissionRejectsUnrelatedOrMalformedSuggestions(t *testing.T) {
	for _, raw := range []string{
		`{"type":"addRules","destination":"localSettings","behavior":"allow","rules":[{"toolName":"Write","ruleContent":"/**"}]}`,
		`{"type":"setMode","destination":"session","mode":"bypassPermissions"}`,
		`{"type":"addRules","destination":"invented","behavior":"allow","rules":[{"toolName":"Bash","ruleContent":"ls:*"}]}`,
	} {
		options, _ := permissionOptions(claudecode.PermissionRequest{ToolName: "Bash", Suggestions: []json.RawMessage{json.RawMessage(raw)}}, "/project")
		if len(options) != 2 {
			t.Fatal(options)
		}
	}
}

func TestAskUserQuestionAnswers(t *testing.T) {
	s, _, _ := boundSession(t)
	s.turnCtx = t.Context()
	s.io.Ask = func(_ context.Context, a agent.Approval) (json.RawMessage, error) {
		if len(a.Questions) != 2 || a.Questions[0].CustomID != "question_0_custom" {
			t.Fatal(a)
		}
		return json.RawMessage(`{"question_0":"One","question_0_custom":"a note","question_1":["A, B"],"question_1_custom":"C"}`), nil
	}
	result, err := s.respond(t.Context(), claudecode.PermissionRequest{ToolName: "AskUserQuestion", ToolUseID: "ask", Input: json.RawMessage(`{"questions":[{"question":"First?","options":[{"label":"One"}]},{"question":"Second?","multiSelect":true,"options":[{"label":"A, B"}]}]}`)})
	if err != nil {
		t.Fatal(err)
	}
	var input struct {
		Answers     map[string]string `json:"answers"`
		Annotations map[string]struct {
			Notes string `json:"notes"`
		} `json:"annotations"`
	}
	if err = json.Unmarshal(result.UpdatedInput, &input); err != nil {
		t.Fatal(err)
	}
	if input.Answers["First?"] != "One" || input.Annotations["First?"].Notes != "a note" || input.Answers["Second?"] != `"A, B", C` {
		t.Fatal(string(result.UpdatedInput))
	}
}

func TestApprovalOnlyAcceptsOfferedOption(t *testing.T) {
	s, _, _ := boundSession(t)
	s.turnCtx = t.Context()
	s.io.Ask = func(context.Context, agent.Approval) (json.RawMessage, error) {
		return json.RawMessage(`"allow-with-updates"`), nil
	}
	_, err := s.respond(t.Context(), claudecode.PermissionRequest{ToolName: "Bash", Input: json.RawMessage(`{}`)})
	if err == nil {
		t.Fatal("accepted an option not offered")
	}
}

func TestExitPlanOptionsRespectBypassAndModel(t *testing.T) {
	s, _, _ := boundSession(t)
	s.allowBypass = true
	s.prePlanMode = "bypassPermissions"
	s.lastModel = "sonnet"
	s.catalog.Models = []claudecode.Model{{Value: "sonnet", SupportsAutoMode: false}}
	options, updates := s.exitPlanOptions(claudecode.PermissionRequest{})
	if options[0].OptionID != "exit-plan-bypass" || !strings.Contains(string(updates["exit-plan-auto"][0]), "acceptEdits") {
		t.Fatalf("%#v %#v", options, updates)
	}
	s.allowBypass = false
	options, _ = s.exitPlanOptions(claudecode.PermissionRequest{})
	if slices.ContainsFunc(options, func(o agent.ApprovalOption) bool { return o.OptionID == "exit-plan-bypass" }) {
		t.Fatal(options)
	}
}

func TestOwnToolUsesBoundSession(t *testing.T) {
	s, _, _ := boundSession(t)
	s.io.Ask = func(context.Context, agent.Approval) (json.RawMessage, error) {
		t.Fatal("asked about own tool")
		return nil, nil
	}
	result, err := s.respond(t.Context(), claudecode.PermissionRequest{ToolName: "mcp__repogo__browser", Input: json.RawMessage(`{}`), ToolUseID: "own"})
	if err != nil || result.Behavior != "allow" {
		t.Fatalf("%#v %v", result, err)
	}
}

func TestDiagnosticsRedactCommonCredentials(t *testing.T) {
	out := diagnosticStderr("failure: api_key=example-secret Authorization: Bearer abc-def sk-ant-example")
	for _, secret := range []string{"example-secret", "abc-def", "sk-ant-example"} {
		if strings.Contains(out, secret) {
			t.Fatalf("unredacted credential")
		}
	}
	if !strings.Contains(out, "failure") {
		t.Fatal("lost diagnostic")
	}
}
