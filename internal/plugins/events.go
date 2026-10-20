package plugins

import (
	"encoding/json"

	"github.com/repogoapp/host/internal/emit"
	"github.com/repogoapp/host/internal/outcome"
)

func init() {
	emit.Register(Data{}, Logged{})
}

// Logged says plugins wrote console lines, naming them, so a card showing
// plugins.logs reloads.
type Logged struct {
	Plugins []string `json:"plugins"`
}

func (Logged) Method() string { return "plugins.logged" }

// Subscribed is a subscription's first answer. Revision counts its answers, so
// a device keeps the newest when pushes and a reply cross. Stream is a stream
// method's: its result starts empty and each Data adds items.
type Subscribed struct {
	Subscription string           `json:"subscription"`
	Revision     int              `json:"revision"`
	Result       json.RawMessage  `json:"result,omitempty"`
	Error        *outcome.Failure `json:"error,omitempty"`
	Stream       bool             `json:"stream,omitempty"`
}

// Data is a subscription's news. A query's Result replaces the last, sent only
// when it changed. A stream's Items add to what it holds, one revision after the
// last, until Done (with Error when it failed); its Result means it restarted.
type Data struct {
	Subscription string           `json:"subscription"`
	Revision     int              `json:"revision"`
	Result       json.RawMessage  `json:"result,omitempty"`
	Items        json.RawMessage  `json:"items,omitempty"`
	Done         bool             `json:"done,omitempty"`
	Error        *outcome.Failure `json:"error,omitempty"`
}

func (Data) Method() string { return "plugins.data" }
