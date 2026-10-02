package mcp

import "github.com/repogo/host/internal/emit"

func init() {
	emit.Register(Changed{})
}

// Changed is the whole list after any change, sent to every paired device, so
// a toggle on one phone moves the switch on the other. Whole rather than a
// delta: a handful of rows, and a missed push is fixed by the next.
type Changed struct {
	Servers  []Server  `json:"servers" wire:"array"`
	Builtins []Builtin `json:"builtins" wire:"array"`
}

func (Changed) Method() string { return "mcp.changed" }
