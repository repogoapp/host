package claude

import (
	"regexp"
	"strings"
)

var diagnosticSecrets = regexp.MustCompile(`(?i)(bearer\s+|(?:api[_-]?key|access[_-]?token|refresh[_-]?token|authorization|password|secret)["']?\s*[:=]\s*["']?)[^\s,"'&]+`)
var diagnosticTokens = regexp.MustCompile(`\bsk-[a-zA-Z0-9_-]+`)
var diagnosticBearer = regexp.MustCompile(`(?i)bearer\s+[^\s,"'&]+`)

func diagnosticStderr(stderr string) string {
	stderr = diagnosticBearer.ReplaceAllString(stderr, "Bearer [redacted]")
	stderr = diagnosticSecrets.ReplaceAllString(stderr, "${1}[redacted]")
	stderr = diagnosticTokens.ReplaceAllString(stderr, "[redacted]")
	return strings.TrimSpace(stderr)
}
