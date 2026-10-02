// Package native links the Rust transcript parser (the rust/ directory beside it)
// into the host. Built with `-tags native` after `make native` has produced
// lib/librepogo_import.a; without the tag the pure-Go parser is used and
// no Rust toolchain is needed.
package native

// Parser picks which transcript format the Rust parser reads. Providers pass
// their own; the generic binding knows only the ABI.
type Parser uint32

// The parsers import.h names, REPOGO_AGENT_CLAUDE and REPOGO_AGENT_CODEX.
// ParseFile maps each onto its C constant, so these values never cross the
// ABI themselves.
const (
	ParserClaude Parser = iota
	ParserCodex
)

// Usage is one API request's token usage as the Rust parser read it; the
// fields mirror shipping.Record, which the providers convert it to.
type Usage struct {
	Key, Session, Model                                           string
	At, Uncached, Cached, Creation, Creation1h, Output, Reasoning int64
	Fast, HasReported                                             bool
	ReportedUSD                                                   float64
}
