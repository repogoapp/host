package plugins

import (
	"encoding/json"

	"github.com/repogoapp/host/internal/emit"
)

func init() {
	emit.Register(Data{})
}

// Failure is a plugin's own failure: a code, a sentence, and messages keyed by the
// input field they're about ("/status"), so a form shows them under its fields.
type Failure struct {
	Code        string            `json:"code"`
	Message     string            `json:"message"`
	FieldErrors map[string]string `json:"field_errors,omitempty"`
}

// Answer is a method's result, or the plugin's error as data: a plugin refusing
// input is an answer to show, not a failed call.
type Answer struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Failure        `json:"error,omitempty"`
}

// Subscribed is a subscription's first answer. Revision counts its answers, so
// a device keeps the newest when pushes and a reply cross. Stream is a stream
// method's: its result starts empty and each Data adds items.
type Subscribed struct {
	Subscription string          `json:"subscription"`
	Revision     int             `json:"revision"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        *Failure        `json:"error,omitempty"`
	Stream       bool            `json:"stream,omitempty"`
}

// Data is a subscription's news. A query's Result replaces the last, sent only
// when it changed. A stream's Items add to what it holds, one revision after the
// last, until Done (with Error when it failed); its Result means it restarted.
type Data struct {
	Subscription string          `json:"subscription"`
	Revision     int             `json:"revision"`
	Result       json.RawMessage `json:"result,omitempty"`
	Items        json.RawMessage `json:"items,omitempty"`
	Done         bool            `json:"done,omitempty"`
	Error        *Failure        `json:"error,omitempty"`
}

func (Data) Method() string { return "plugins.data" }
