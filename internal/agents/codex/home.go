package codex

import "github.com/repogoapp/host/internal/agent"

// defaultHome is Codex's config directory: CODEX_HOME or ~/.codex.
func defaultHome() string { return agent.ConfigHome("CODEX_HOME", ".codex") }
