package codexappserver

import (
	"context"
	"encoding/json"

	"github.com/repogo/host/internal/stdiorpc"
)

type Client struct{ *stdiorpc.Client }
type Exit = stdiorpc.Exit
type Error = stdiorpc.Error
type Handlers struct {
	Request func(context.Context, Request) (any, error)
}

func Start(ctx context.Context, opts Options, handlers Handlers) (*Client, error) {
	args, err := opts.args()
	if err != nil {
		return nil, err
	}
	client, err := stdiorpc.Start(ctx, stdiorpc.Options{Executable: opts.Executable, Cwd: opts.Cwd, Args: args, Env: opts.Env}, stdiorpc.Handlers{
		Request: handlers.Request,
		Resolved: func(method string, raw json.RawMessage) json.RawMessage {
			if method != MethodServerRequestResolved {
				return nil
			}
			var p ResolvedNotification
			if json.Unmarshal(raw, &p) != nil {
				return nil
			}
			return p.RequestID
		},
	})
	if err != nil {
		return nil, err
	}
	return &Client{Client: client}, nil
}
