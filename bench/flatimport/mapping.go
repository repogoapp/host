package flatimport

import (
	"github.com/repogo/host/bench/flatimport/fb"
	"github.com/repogo/host/internal/agent"
)

func packToolCall(v *agent.ToolCall) *fb.ToolCallT {
	if v == nil {
		return nil
	}
	o := &fb.ToolCallT{}
	o.CallId = v.CallID
	o.Name = v.Name
	o.Input = v.Input
	o.Output = v.Output
	o.IsError = v.IsError
	return o
}
func unpackToolCall(v *fb.ToolCallT) *agent.ToolCall {
	if v == nil {
		return nil
	}
	o := &agent.ToolCall{}
	o.CallID = v.CallId
	o.Name = v.Name
	o.Input = v.Input
	o.Output = v.Output
	o.IsError = v.IsError
	return o
}
func packUsage(v *agent.Usage) *fb.UsageT {
	if v == nil {
		return nil
	}
	o := &fb.UsageT{}
	o.InputTokens = v.InputTokens
	o.OutputTokens = v.OutputTokens
	o.CacheReadTokens = v.CacheReadTokens
	o.CacheCreationTokens = v.CacheCreationTokens
	o.CostUsd = v.CostUSD
	o.DurationMs = v.DurationMS
	o.ContextUsed = v.ContextUsed
	o.ContextSize = v.ContextSize
	return o
}
func unpackUsage(v *fb.UsageT) *agent.Usage {
	if v == nil {
		return nil
	}
	o := &agent.Usage{}
	o.InputTokens = v.InputTokens
	o.OutputTokens = v.OutputTokens
	o.CacheReadTokens = v.CacheReadTokens
	o.CacheCreationTokens = v.CacheCreationTokens
	o.CostUSD = v.CostUsd
	o.DurationMS = v.DurationMs
	o.ContextUsed = v.ContextUsed
	o.ContextSize = v.ContextSize
	return o
}
func packApprovalOption(v *agent.ApprovalOption) *fb.ApprovalOptionT {
	if v == nil {
		return nil
	}
	o := &fb.ApprovalOptionT{}
	o.OptionId = v.OptionID
	o.Name = v.Name
	o.Kind = v.Kind
	return o
}
func unpackApprovalOption(v *fb.ApprovalOptionT) *agent.ApprovalOption {
	if v == nil {
		return nil
	}
	o := &agent.ApprovalOption{}
	o.OptionID = v.OptionId
	o.Name = v.Name
	o.Kind = v.Kind
	return o
}
func packQuestionOption(v *agent.QuestionOption) *fb.QuestionOptionT {
	if v == nil {
		return nil
	}
	o := &fb.QuestionOptionT{}
	o.Label = v.Label
	o.Description = v.Description
	return o
}
func unpackQuestionOption(v *fb.QuestionOptionT) *agent.QuestionOption {
	if v == nil {
		return nil
	}
	o := &agent.QuestionOption{}
	o.Label = v.Label
	o.Description = v.Description
	return o
}
func packQuestion(v *agent.Question) *fb.QuestionT {
	if v == nil {
		return nil
	}
	o := &fb.QuestionT{}
	o.Id = v.ID
	o.CustomId = v.CustomID
	o.Header = v.Header
	o.Text = v.Text
	o.MultiSelect = v.MultiSelect
	if v.Options != nil {
		o.Options = make([]*fb.QuestionOptionT, 0, len(v.Options))
		for _, x := range v.Options {
			o.Options = append(o.Options, packQuestionOption(&x))
		}
	}
	return o
}
func unpackQuestion(v *fb.QuestionT) *agent.Question {
	if v == nil {
		return nil
	}
	o := &agent.Question{}
	o.ID = v.Id
	o.CustomID = v.CustomId
	o.Header = v.Header
	o.Text = v.Text
	o.MultiSelect = v.MultiSelect
	if v.Options != nil {
		o.Options = make([]agent.QuestionOption, 0, len(v.Options))
		for _, x := range v.Options {
			o.Options = append(o.Options, *unpackQuestionOption(x))
		}
	}
	return o
}
func packApproval(v *agent.Approval) *fb.ApprovalT {
	if v == nil {
		return nil
	}
	o := &fb.ApprovalT{}
	o.CallId = v.CallID
	o.Title = v.Title
	o.Kind = v.Kind
	o.Input = v.Input
	if v.Options != nil {
		o.Options = make([]*fb.ApprovalOptionT, 0, len(v.Options))
		for _, x := range v.Options {
			o.Options = append(o.Options, packApprovalOption(&x))
		}
	}
	if v.Questions != nil {
		o.Questions = make([]*fb.QuestionT, 0, len(v.Questions))
		for _, x := range v.Questions {
			o.Questions = append(o.Questions, packQuestion(&x))
		}
	}
	return o
}
func unpackApproval(v *fb.ApprovalT) *agent.Approval {
	if v == nil {
		return nil
	}
	o := &agent.Approval{}
	o.CallID = v.CallId
	o.Title = v.Title
	o.Kind = v.Kind
	o.Input = v.Input
	if v.Options != nil {
		o.Options = make([]agent.ApprovalOption, 0, len(v.Options))
		for _, x := range v.Options {
			o.Options = append(o.Options, *unpackApprovalOption(x))
		}
	}
	if v.Questions != nil {
		o.Questions = make([]agent.Question, 0, len(v.Questions))
		for _, x := range v.Questions {
			o.Questions = append(o.Questions, *unpackQuestion(x))
		}
	}
	return o
}
func packEvent(v *agent.Event) *fb.EventT {
	if v == nil {
		return nil
	}
	o := &fb.EventT{}
	o.TurnId = v.TurnID
	o.Seq = v.Seq
	o.Kind = string(v.Kind)
	o.At = v.At
	o.Text = v.Text
	o.Tool = packToolCall(v.Tool)
	o.SessionId = v.SessionID
	o.Approval = packApproval(v.Approval)
	o.Usage = packUsage(v.Usage)
	o.Error = v.Error
	return o
}
func unpackEvent(v *fb.EventT) *agent.Event {
	if v == nil {
		return nil
	}
	o := &agent.Event{}
	o.TurnID = v.TurnId
	o.Seq = v.Seq
	o.Kind = agent.EventKind(v.Kind)
	o.At = v.At
	o.Text = v.Text
	o.Tool = unpackToolCall(v.Tool)
	o.SessionID = v.SessionId
	o.Approval = unpackApproval(v.Approval)
	o.Usage = unpackUsage(v.Usage)
	o.Error = v.Error
	return o
}
