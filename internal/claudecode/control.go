package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

type controlResponse struct {
	Subtype   string          `json:"subtype"`
	RequestID string          `json:"request_id"`
	Response  json.RawMessage `json:"response,omitempty"`
	Error     string          `json:"error,omitempty"`
}

type ControlError struct{ Message string }

func (e *ControlError) Error() string { return "claudecode: control: " + e.Message }

type Model struct {
	Value            string   `json:"value"`
	ResolvedModel    string   `json:"resolvedModel"`
	SupportsEffort   bool     `json:"supportsEffort"`
	Efforts          []string `json:"supportedEffortLevels"`
	SupportsFastMode bool     `json:"supportsFastMode"`
	SupportsAutoMode bool     `json:"supportsAutoMode"`
}

type InitializeResponse struct {
	Models []Model `json:"models"`
}

func (c *Client) requestControl(ctx context.Context, request any, out any) error {
	c.mu.Lock()
	c.nextID++
	id := fmt.Sprintf("host-%d", c.nextID)
	ch := make(chan controlResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.writeMessage(ctx, struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   any    `json:"request"`
	}{"control_request", id, request}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		return fmt.Errorf("claudecode: process exited (%d): %w", c.Exit().Code, errors.New("control response missing"))
	case response := <-ch:
		if response.Subtype != "success" {
			return &ControlError{Message: response.Error}
		}
		if out != nil {
			return json.Unmarshal(response.Response, out)
		}
		return nil
	}
}

func (c *Client) Initialize(ctx context.Context) (InitializeResponse, error) {
	var out InitializeResponse
	err := c.requestControl(ctx, map[string]any{"subtype": "initialize"}, &out)
	return out, err
}
func (c *Client) Interrupt(ctx context.Context) error {
	return c.requestControl(ctx, map[string]any{"subtype": "interrupt"}, nil)
}
func (c *Client) SetModel(ctx context.Context, model string) error {
	return c.requestControl(ctx, map[string]any{"subtype": "set_model", "model": model}, nil)
}
func (c *Client) SetPermissionMode(ctx context.Context, mode string) error {
	return c.requestControl(ctx, map[string]any{"subtype": "set_permission_mode", "mode": mode}, nil)
}
func (c *Client) ApplyFlagSettings(ctx context.Context, settings map[string]any) error {
	return c.requestControl(ctx, map[string]any{"subtype": "apply_flag_settings", "settings": settings}, nil)
}

func (c *Client) SetMaxThinkingTokens(ctx context.Context, tokens *int) error {
	return c.requestControl(ctx, map[string]any{"subtype": "set_max_thinking_tokens", "max_thinking_tokens": tokens}, nil)
}

func (c *Client) StopTask(ctx context.Context, id string) error {
	return c.requestControl(ctx, map[string]any{"subtype": "stop_task", "task_id": id}, nil)
}

type MCPResult struct {
	Added   []string          `json:"added"`
	Removed []string          `json:"removed"`
	Errors  map[string]string `json:"errors"`
}

func (c *Client) SetMCPServers(ctx context.Context, servers map[string]MCPServer) (MCPResult, error) {
	var result MCPResult
	err := c.requestControl(ctx, map[string]any{"subtype": "mcp_set_servers", "servers": servers}, &result)
	return result, err
}

func (c *Client) handleControlRequest(id string, raw json.RawMessage) {
	if id == "" {
		c.fail(errors.New("claudecode: inbound request has no id"))
		return
	}
	ctx, cancel := context.WithCancel(c.ctx)
	c.mu.Lock()
	if len(c.requests) >= 1024 {
		c.mu.Unlock()
		cancel()
		c.fail(errors.New("claudecode: too many pending inbound requests"))
		return
	}
	if _, exists := c.requests[id]; exists {
		c.mu.Unlock()
		cancel()
		c.fail(errors.New("claudecode: duplicate inbound request id"))
		return
	}
	c.requests[id] = cancel
	c.mu.Unlock()
	go func() {
		defer cancel()
		defer func() { c.mu.Lock(); delete(c.requests, id); c.mu.Unlock() }()
		result, err := c.dispatchControl(ctx, raw)
		response := controlResponse{Subtype: "success", RequestID: id}
		if err != nil {
			response.Subtype = "error"
			response.Error = err.Error()
		} else {
			response.Response, err = json.Marshal(result)
		}
		if err != nil && response.Subtype == "success" {
			response.Subtype = "error"
			response.Error = err.Error()
		}
		if ctx.Err() != nil {
			return
		}
		if err := c.writeMessage(ctx, struct {
			Type     string          `json:"type"`
			Response controlResponse `json:"response"`
		}{"control_response", response}); err != nil {
			c.fail(err)
		}
	}()
}

func (c *Client) dispatchControl(ctx context.Context, raw json.RawMessage) (any, error) {
	var request struct {
		Subtype string `json:"subtype"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, err
	}
	switch request.Subtype {
	case "can_use_tool":
		var p PermissionRequest
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if c.handlers.CanUseTool == nil {
			return PermissionResult{Behavior: "deny", Message: "No permission handler"}, nil
		}
		return c.handlers.CanUseTool(ctx, p)
	case "elicitation":
		var e ElicitationRequest
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, err
		}
		if c.handlers.Elicitation == nil {
			return ElicitationResult{Action: "decline"}, nil
		}
		return c.handlers.Elicitation(ctx, e)
	default:
		return nil, fmt.Errorf("unsupported Claude control request: %s", request.Subtype)
	}
}
