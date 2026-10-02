//go:build !native

package native

import (
	"errors"

	"github.com/repogo/host/internal/agent"
)

// Available reports whether this build carries the native parser.
const Available = false

// ParseFile is never called in a build without the native tag; session's
// readSegment checks Available first.
func ParseFile(Parser, string, string) ([]agent.Event, error) {
	return nil, errors.New("native parser not built in")
}

// ScanUsage is never called in a build without the native tag; the usage
// ledger checks Available first.
func ScanUsage(Parser, string) ([]Usage, error) {
	return nil, errors.New("native parser not built in")
}
