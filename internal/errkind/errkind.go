// Package errkind is how a core package says what kind of failure a sentinel
// is, so the router maps four kinds onto the wire instead of every package's
// errors by name.
package errkind

import (
	"errors"
	"fmt"
)

type Kind uint8

const (
	// Internal is any error that carries no kind: the host's own fault.
	Internal Kind = iota
	Invalid
	NotFound
	Denied
	Unavailable
)

// Error is a sentinel with a kind. errors.Is matches it by identity, and also
// against its kind's root (ErrInvalid, …), so a caller can test either.
type Error struct {
	kind Kind
	msg  string
}

func (e *Error) Error() string { return e.msg }

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t == roots[e.kind]
}

// The roots: one per kind, what errors.Is checks a kind against.
var (
	ErrInvalid     = &Error{Invalid, "invalid parameters"}
	ErrNotFound    = &Error{NotFound, "not found"}
	ErrDenied      = &Error{Denied, "not permitted"}
	ErrUnavailable = &Error{Unavailable, "unavailable"}
)

var roots = map[Kind]*Error{
	Invalid: ErrInvalid, NotFound: ErrNotFound, Denied: ErrDenied, Unavailable: ErrUnavailable,
}

func New(k Kind, msg string) *Error { return &Error{k, msg} }

// Errorf is e with a formatted detail after its message, still matching e.
func (e *Error) Errorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", e, fmt.Sprintf(format, args...))
}

// Of is the kind of the first kinded error in err's chain; Internal if none.
func Of(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.kind
	}
	return Internal
}
