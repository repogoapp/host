package terminal

import "github.com/repogo/host/internal/emit"

// Every event this package emits, declared for the catalog. Sent per device
// to whoever is attached or watching, never broadcast to a room: a shell's
// output belongs to the devices that asked for it.

func init() {
	emit.Register(Changed{}, Output{}, Exit{})
}

// Changed is a project's whole tab set, sent to every device watching that
// project. A snapshot, so it cannot drift the way a replayed delta can.
type Changed struct {
	Cwd      string `json:"cwd"`
	Sessions []Info `json:"sessions" wire:"array"`
}

func (Changed) Method() string { return "terminals.changed" }

// Output is bytes the shell wrote, coalesced over a short window.
type Output struct {
	SessionID string `json:"session_id"`
	Data      []byte `json:"data"`
}

func (Output) Method() string { return "terminals.output" }

// Exit is the shell ending, with what it left in the last chunk already sent.
type Exit struct {
	SessionID string `json:"session_id"`
	ExitCode  int    `json:"exit_code"`
}

func (Exit) Method() string { return "terminals.exit" }
