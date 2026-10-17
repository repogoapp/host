package stdiorpc

import "encoding/json"

type Options struct {
	Executable, Cwd string
	Args, Env       []string
	Version         string
	// LogLines keeps a stdout line that isn't JSON with stderr instead of
	// failing: some servers (ACP agents such as eve) print progress there.
	LogLines bool
}

type Request struct {
	ID     json.RawMessage
	Method string
	Params json.RawMessage
}

type Notification struct {
	Method string
	Params json.RawMessage
}
