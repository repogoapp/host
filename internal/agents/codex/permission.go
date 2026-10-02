package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/codexappserver"
)

// respond answers what Codex asks of a person. A subagent's request comes
// on the same connection and is answered through the chat's turn, since the
// work it blocks is that turn's.
func (s *liveSession) respond(ctx context.Context, r codexappserver.Request) (any, error) {
	switch r.Method {
	case codexappserver.MethodCommandApproval:
		var p codexappserver.CommandApproval
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return nil, err
		}
		return s.commandApproval(ctx, p)
	case codexappserver.MethodFileChangeApproval:
		var p codexappserver.FileChangeApproval
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return nil, err
		}
		return s.fileChangeApproval(ctx, p)
	case codexappserver.MethodPermissionsApproval:
		var p codexappserver.PermissionsApproval
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return nil, err
		}
		return s.permissionsApproval(ctx, p)
	case codexappserver.MethodUserInput:
		var p codexappserver.UserInput
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return nil, err
		}
		return s.userInput(ctx, p)
	case codexappserver.MethodElicitation:
		var p codexappserver.Elicitation
		if err := json.Unmarshal(r.Params, &p); err != nil {
			return nil, err
		}
		return s.elicitation(ctx, string(r.ID), p)
	default:
		return nil, fmt.Errorf("RepoGo does not answer Codex's %s", r.Method)
	}
}

// decision is one answer an approval offers: the option the phone shows and
// what goes back to Codex when it is picked.
type decision struct {
	option agent.ApprovalOption
	reply  any
}

// allDecisions is what an approval offers when Codex does not list them.
var allDecisions = []json.RawMessage{
	json.RawMessage(`"accept"`), json.RawMessage(`"acceptForSession"`),
	json.RawMessage(`"decline"`), json.RawMessage(`"cancel"`),
}

// decisions turns Codex's available decisions into options, in its order.
// Option ids are the decisions' own names; one the host cannot show is left out.
func decisions(available []json.RawMessage, amendment []string) []decision {
	if len(available) == 0 {
		available = allDecisions
	}
	var out []decision
	for _, raw := range available {
		var name string
		if json.Unmarshal(raw, &name) == nil {
			if d, ok := plainDecision(name); ok {
				out = append(out, d)
			}
			continue
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			continue
		}
		if _, ok := object["acceptWithExecpolicyAmendment"]; ok {
			label := "Yes, and don't ask again for commands like this"
			if len(amendment) > 0 {
				label = "Yes, and don't ask again for `" + strings.Join(amendment, " ") + "`"
			}
			out = append(out, decision{
				option: agent.ApprovalOption{OptionID: "acceptWithExecpolicyAmendment", Name: label, Kind: "allow_always"},
				reply:  raw,
			})
		}
	}
	return out
}

func plainDecision(name string) (decision, bool) {
	option := agent.ApprovalOption{OptionID: name}
	switch name {
	case "accept":
		option.Name, option.Kind = "Yes", "allow_once"
	case "acceptForSession":
		option.Name, option.Kind = "Yes, and don't ask again this session", "allow_always"
	case "decline":
		option.Name, option.Kind = "No", "reject_once"
	case "cancel":
		option.Name, option.Kind = "No, and stop", "reject_once"
	default:
		return decision{}, false
	}
	return decision{option: option, reply: name}, true
}

// decide asks a person to pick one of the decisions. A withdrawn question
// cancels, which interrupts the turn rather than letting it act.
func (s *liveSession) decide(ctx context.Context, approval agent.Approval, offered []decision) (any, error) {
	for _, d := range offered {
		approval.Options = append(approval.Options, d.option)
	}
	answer, err := s.ask(ctx, approval)
	if err != nil {
		return "cancel", nil
	}
	var picked string
	if err := json.Unmarshal(answer, &picked); err != nil {
		return nil, err
	}
	for _, d := range offered {
		if d.option.OptionID == picked {
			return d.reply, nil
		}
	}
	return nil, errors.New("approval option was not offered")
}

func (s *liveSession) commandApproval(ctx context.Context, p codexappserver.CommandApproval) (codexappserver.DecisionResponse, error) {
	command, cwd, reason := deref(p.Command), deref(p.Cwd), deref(p.Reason)
	input, _ := json.Marshal(map[string]string{"command": command, "cwd": cwd, "reason": reason})
	title := command
	if title == "" {
		title = reason
	}
	reply, err := s.decide(ctx, agent.Approval{CallID: p.ItemID, Title: title, Kind: "execute", Input: input},
		decisions(p.AvailableDecisions, p.ProposedExecpolicyAmendment))
	return codexappserver.DecisionResponse{Decision: reply}, err
}

func (s *liveSession) fileChangeApproval(ctx context.Context, p codexappserver.FileChangeApproval) (codexappserver.DecisionResponse, error) {
	s.mu.Lock()
	changes := s.changes[p.ItemID]
	s.mu.Unlock()
	reason := deref(p.Reason)
	input, _ := json.Marshal(map[string]any{"changes": changes, "reason": reason})
	title := reason
	if title == "" {
		title = "Edit files"
	}
	reply, err := s.decide(ctx, agent.Approval{CallID: p.ItemID, Title: title, Kind: "edit", Input: input},
		decisions(nil, nil))
	return codexappserver.DecisionResponse{Decision: reply}, err
}

// permissionsApproval grants the access Codex asked for, for the turn or the
// session, or nothing.
func (s *liveSession) permissionsApproval(ctx context.Context, p codexappserver.PermissionsApproval) (codexappserver.PermissionsResponse, error) {
	title := deref(p.Reason)
	if title == "" {
		title = "Allow access outside the sandbox"
	}
	offered := []decision{
		{option: agent.ApprovalOption{OptionID: "turn", Name: "Yes, for this turn", Kind: "allow_once"}},
		{option: agent.ApprovalOption{OptionID: "session", Name: "Yes, and don't ask again this session", Kind: "allow_always"}},
		{option: agent.ApprovalOption{OptionID: "decline", Name: "No", Kind: "reject_once"}},
	}
	for i := range offered {
		offered[i].reply = offered[i].option.OptionID
	}
	picked, err := s.decide(ctx, agent.Approval{CallID: p.ItemID, Title: title, Kind: "permissions", Input: p.Permissions}, offered)
	if err != nil {
		return codexappserver.PermissionsResponse{}, err
	}
	switch picked {
	case "turn", "session":
		return codexappserver.PermissionsResponse{Permissions: p.Permissions, Scope: picked.(string)}, nil
	default:
		return codexappserver.PermissionsResponse{Permissions: json.RawMessage(`{}`)}, nil
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
