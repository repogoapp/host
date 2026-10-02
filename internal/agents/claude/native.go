package claude

import (
	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/session/native"
)

// ParseFile reads a whole transcript with the Rust parser. The session store
// tries it first and falls back to Parse line by line when it fails, as it
// always does in a build without the native library.
func (c *Sessions) ParseFile(path, sessionID string) ([]agent.Event, error) {
	return native.ParseFile(native.ParserClaude, path, sessionID)
}
