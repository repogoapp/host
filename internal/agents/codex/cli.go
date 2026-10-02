package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/clitool"
	"github.com/repogo/host/internal/codexappserver"
)

// oneShotTimeout bounds a throwaway app server; its callers cache the answer
// and have a fallback, so a slow CLI costs a stale answer, not a hung request.
const oneShotTimeout = 20 * time.Second

// executable is the codex a turn and a one-shot run: the native install when
// there is one, since it updates itself, else whatever PATH finds.
func executable() string { return clitool.PreferredExecutable(definition().Tool.Spec) }

// appServer runs methods (key -> method) against a throwaway app server and
// answers each by key; nil marks one Codex answered with an error.
func appServer(ctx context.Context, methods map[string]string) (map[string]json.RawMessage, bool) {
	calls := make(map[string]rpcCall, len(methods))
	for key, method := range methods {
		calls[key] = rpcCall{method: method}
	}
	return appServerCalls(ctx, calls)
}

// appServerCall runs one method that takes params, and returns its result.
func appServerCall(ctx context.Context, method string, params any) (json.RawMessage, bool) {
	res, ok := appServerCalls(ctx, map[string]rpcCall{"call": {method: method, params: params}})
	return res["call"], ok && len(res["call"]) > 0
}

// rpcCall is one request; nil params sends an empty object.
type rpcCall struct {
	method string
	params any
}

// appServerCalls reports false when the server could not be reached at all;
// a method Codex refused is a nil entry, not a failure of the others.
func appServerCalls(ctx context.Context, calls map[string]rpcCall) (map[string]json.RawMessage, bool) {
	ctx, cancel := context.WithTimeout(ctx, oneShotTimeout)
	defer cancel()
	client, err := codexappserver.Start(ctx, codexappserver.Options{
		// A neutral directory: the answer is the CLI's, not a project's.
		Executable: executable(), Cwd: os.TempDir(), Env: agent.ChildEnv(),
	}, codexappserver.Handlers{})
	if err != nil {
		return nil, false
	}
	defer client.Close()
	// Notifications are not read; draining them keeps the buffer from filling.
	go func() {
		for range client.Notifications() {
		}
	}()
	if client.Initialize(ctx) != nil {
		return nil, false
	}
	out := make(map[string]json.RawMessage, len(calls))
	for key, call := range calls {
		result, err := client.Raw(ctx, call.method, call.params)
		var refused *codexappserver.Error
		switch {
		case err == nil:
			out[key] = result
		case errors.As(err, &refused):
			out[key] = nil
		default:
			return out, false
		}
	}
	return out, true
}
