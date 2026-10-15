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
// a device keeps the newest when pushes and a reply cross.
type Subscribed struct {
	Subscription string          `json:"subscription"`
	Revision     int             `json:"revision"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        *Failure        `json:"error,omitempty"`
}

// Data is a subscription's new answer, sent to the device that subscribed only
// when it differs from the last one.
type Data struct {
	Subscription string          `json:"subscription"`
	Revision     int             `json:"revision"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        *Failure        `json:"error,omitempty"`
}

func (Data) Method() string { return "plugins.data" }
