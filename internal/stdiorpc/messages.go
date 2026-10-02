package stdiorpc

import "encoding/json"

type Options struct {
	Executable, Cwd string
	Args, Env       []string
	Version         string
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
