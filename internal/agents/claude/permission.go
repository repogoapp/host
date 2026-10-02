package claude

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/claudecode"
)

func denied(p claudecode.PermissionRequest, message string, interrupt bool) claudecode.PermissionResult {
	return claudecode.PermissionResult{Behavior: "deny", Message: message, Interrupt: interrupt, ToolUseID: p.ToolUseID, DecisionClassification: "user_reject"}
}

// respond answers Claude's can_use_tool: RepoGo's own tools run, and the rest
// wait on a person, whose pick becomes Claude's permission result.
func (s *liveSession) respond(ctx context.Context, p claudecode.PermissionRequest) (claudecode.PermissionResult, error) {
	if p.ToolName == "AskUserQuestion" {
		return s.askUserQuestion(ctx, p)
	}
	if strings.HasPrefix(p.ToolName, "mcp__"+agent.ToolsServer+"__") {
		return claudecode.PermissionResult{Behavior: "allow", UpdatedInput: p.Input, ToolUseID: p.ToolUseID}, nil
	}
	var options []agent.ApprovalOption
	var updates map[string][]json.RawMessage
	if p.ToolName == "ExitPlanMode" {
		options, updates = s.exitPlanOptions(p)
	} else {
		options, updates = permissionOptions(p, s.cwd)
	}
	answer, err := s.ask(ctx, agent.Approval{CallID: p.ToolUseID, Title: p.ToolName, Kind: liveToolName(p.ToolName), Input: p.Input, Options: options})
	if err != nil {
		return denied(p, "Permission cancelled", true), nil
	}
	var selected string
	if err = json.Unmarshal(answer, &selected); err != nil {
		return claudecode.PermissionResult{}, err
	}
	offered := false
	for _, option := range options {
		if option.OptionID == selected {
			offered = true
			break
		}
	}
	if !offered {
		return claudecode.PermissionResult{}, errors.New("permission option was not offered")
	}
	if selected == "reject" {
		plan := p.ToolName == "ExitPlanMode"
		if plan {
			s.mu.Lock()
			s.planRejected = p.ToolUseID
			s.mu.Unlock()
		}
		return denied(p, "The user rejected this tool use.", plan), nil
	}
	result := claudecode.PermissionResult{Behavior: "allow", UpdatedInput: p.Input, ToolUseID: p.ToolUseID, DecisionClassification: "user_temporary"}
	if changes := updates[selected]; len(changes) > 0 {
		result.UpdatedPermissions = changes
		if selected != "exit-plan-default" {
			result.DecisionClassification = "user_permanent"
		}
		if p.ToolName == "ExitPlanMode" {
			var update struct {
				Mode string `json:"mode"`
			}
			_ = json.Unmarshal(changes[0], &update)
			s.mu.Lock()
			s.mode = update.Mode
			s.mu.Unlock()
		}
	}
	return result, nil
}

func (s *liveSession) exitPlanOptions(p claudecode.PermissionRequest) ([]agent.ApprovalOption, map[string][]json.RawMessage) {
	s.mu.Lock()
	bypass, previous, model := s.allowBypass, s.prePlanMode, s.lastModel
	s.mu.Unlock()
	autoMode := "auto"
	for _, info := range s.catalog.Models {
		if (info.Value == model || info.ResolvedModel == model) && !info.SupportsAutoMode {
			autoMode = "acceptEdits"
		}
	}
	elevated := []agent.ApprovalOption{{OptionID: "exit-plan-auto", Name: "Yes, and use auto mode", Kind: "allow_always"}}
	if bypass {
		option := agent.ApprovalOption{OptionID: "exit-plan-bypass", Name: "Yes, and bypass permissions", Kind: "allow_always"}
		if previous == "bypassPermissions" {
			elevated = append([]agent.ApprovalOption{option}, elevated...)
		} else {
			elevated = append(elevated, option)
		}
	}
	options := append(elevated, agent.ApprovalOption{OptionID: "exit-plan-default", Name: "Yes, manually approve edits", Kind: "allow_once"})
	reject := agent.ApprovalOption{OptionID: "reject", Name: "No, keep planning", Kind: "reject_once"}
	if p.DefaultToNo {
		options = append([]agent.ApprovalOption{reject}, options...)
	} else {
		options = append(options, reject)
	}
	return options, map[string][]json.RawMessage{"exit-plan-auto": permissionModeUpdate(autoMode), "exit-plan-bypass": permissionModeUpdate("bypassPermissions"), "exit-plan-default": permissionModeUpdate("default")}
}

func permissionOptions(p claudecode.PermissionRequest, cwd string) ([]agent.ApprovalOption, map[string][]json.RawMessage) {
	allow := agent.ApprovalOption{OptionID: "allow-once", Name: "Yes", Kind: "allow_once"}
	reject := agent.ApprovalOption{OptionID: "reject", Name: "No", Kind: "reject_once"}
	options := []agent.ApprovalOption{allow}
	updates := map[string][]json.RawMessage{}
	persistent := !p.SuppressAlwaysAllow && (len(p.MatchedAskRule) == 0 || string(p.MatchedAskRule) == "null")
	var input map[string]any
	_ = json.Unmarshal(p.Input, &input)
	add := func(id, label string, changes []json.RawMessage) {
		if len(changes) == 0 {
			return
		}
		options = append(options, agent.ApprovalOption{OptionID: id, Name: label, Kind: "allow_always"})
		updates[id] = changes
	}
	switch p.ToolName {
	case "EnterPlanMode":
		options[0].Name = "Yes, enter plan mode"
		reject.Name = "No, start implementing now"
	case "WebFetch":
		address, _ := input["url"].(string)
		parsed, err := url.Parse(address)
		if persistent && err == nil && parsed.Hostname() != "" {
			add("allow-with-updates", "Yes, and don't ask again for "+parsed.Hostname(), allowRule("WebFetch", "domain:"+parsed.Hostname()))
		}
	case "Skill":
		skill, _ := input["skill"].(string)
		if persistent && skill != "" {
			add("allow-skill-exact", "Yes, and don't ask again for "+skill, allowRule("Skill", skill))
			if prefix, _, ok := strings.Cut(skill, " "); ok {
				add("allow-skill-prefix", "Yes, and don't ask again for "+prefix+":* commands", allowRule("Skill", prefix+":*"))
			}
		}
	default:
		if persistent {
			changes, label := suggestedPermissions(p, cwd)
			add("allow-with-updates", label, changes)
		}
	}
	if p.DefaultToNo {
		options = append([]agent.ApprovalOption{reject}, options...)
	} else {
		options = append(options, reject)
	}
	return options, updates
}

func allowRule(tool, content string) []json.RawMessage {
	rule := map[string]string{"toolName": tool}
	if content != "" {
		rule["ruleContent"] = content
	}
	raw, _ := json.Marshal(map[string]any{"type": "addRules", "destination": "localSettings", "behavior": "allow", "rules": []map[string]string{rule}})
	return []json.RawMessage{raw}
}

func permissionModeUpdate(mode string) []json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"type": "setMode", "mode": mode, "destination": "session"})
	return []json.RawMessage{raw}
}
