// Package outcome is what a plugin method and an MCP tool answer a call with:
// one shape, so a card, an agent and the voice agent read either alike.
package outcome

import "encoding/json"

// Answer is a call's result, or its refusal as data: a method refusing its
// input, or asking first (CONFIRM), is an answer to show, not a failed call.
type Answer struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Failure        `json:"error,omitempty"`
}

// Failure is a refusal: a code, a sentence, and for a plugin messages keyed
// by the input field they're about ("/status"), so a form shows them under its fields.
type Failure struct {
	Code        string            `json:"code"`
	Message     string            `json:"message"`
	FieldErrors map[string]string `json:"field_errors,omitempty"`
}
