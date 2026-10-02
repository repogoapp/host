// Package codexappserver speaks `codex app-server`'s JSON-RPC over stdio: the
// calls the host makes, the notifications Codex streams, and the requests it
// sends back when it needs a person. It knows nothing of RepoGo's types.
package codexappserver

import "errors"

// Options start one `codex app-server` process.
type Options struct {
	Executable, Cwd string
	Env             []string
}

func (o Options) args() ([]string, error) {
	if o.Executable == "" || o.Cwd == "" {
		return nil, errors.New("codexappserver: executable and cwd are required")
	}
	return []string{"app-server"}, nil
}
