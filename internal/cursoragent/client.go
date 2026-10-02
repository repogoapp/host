// Package cursoragent speaks the official Cursor binary's ACP protocol.
package cursoragent

import (
	"context"
	"fmt"
	"github.com/repogo/host/internal/stdiorpc"
)

type Client struct{ *stdiorpc.Client }
type Options struct {
	Executable, Cwd string
	Env             []string
}
type Request = stdiorpc.Request
type Notification = stdiorpc.Notification
type Error = stdiorpc.Error
type Handlers struct {
	Request      func(context.Context, Request) (any, error)
	Notification func(Notification) error
}

func Start(ctx context.Context, opts Options, h Handlers) (*Client, error) {
	c, err := stdiorpc.Start(ctx, stdiorpc.Options{Executable: opts.Executable, Cwd: opts.Cwd, Env: opts.Env, Args: []string{"acp"}, Version: "2.0"}, stdiorpc.Handlers{Request: h.Request, Notification: h.Notification})
	if err != nil {
		return nil, err
	}
	return &Client{Client: c}, nil
}

func (c *Client) Initialize(ctx context.Context) (Initialization, error) {
	var out Initialization
	err := c.Call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": struct{}{}, "clientInfo": map[string]string{"name": "repogo", "version": "0.1.0"}}, &out)
	if err == nil && out.ProtocolVersion != 1 {
		err = fmt.Errorf("Cursor protocol version %d is unsupported", out.ProtocolVersion)
	}
	return out, err
}

func (c *Client) Open(ctx context.Context, p SessionParams) (Session, error) {
	method := "session/new"
	if p.SessionID != "" {
		method = "session/load"
	}
	if p.MCPServers == nil {
		p.MCPServers = []MCPServer{}
	}
	var out Session
	err := c.Call(ctx, method, p, &out)
	if err != nil {
		return out, err
	}
	if p.SessionID != "" {
		if out.SessionID != "" && out.SessionID != p.SessionID {
			return out, fmt.Errorf("Cursor loaded a different session")
		}
		out.SessionID = p.SessionID
	}
	if out.SessionID == "" {
		return out, fmt.Errorf("Cursor returned no session id")
	}
	return out, nil
}

func (c *Client) SetModel(ctx context.Context, id, model string) error {
	return c.Call(ctx, "session/set_model", map[string]string{"sessionId": id, "modelId": model}, nil)
}
func (c *Client) SetMode(ctx context.Context, id, mode string) error {
	return c.Call(ctx, "session/set_mode", map[string]string{"sessionId": id, "modeId": mode}, nil)
}
func (c *Client) Prompt(ctx context.Context, id string, content []Content) (PromptResult, error) {
	var out PromptResult
	err := c.Call(ctx, "session/prompt", map[string]any{"sessionId": id, "prompt": content}, &out)
	return out, err
}
func (c *Client) Cancel(ctx context.Context, id string) error {
	return c.Notify(ctx, "session/cancel", map[string]string{"sessionId": id})
}
