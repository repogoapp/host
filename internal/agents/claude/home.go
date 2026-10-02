package claude

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/repogo/host/internal/agent"
)

// defaultHome is Claude Code's config directory: CLAUDE_CONFIG_DIR or ~/.claude.
func defaultHome() string { return agent.ConfigHome("CLAUDE_CONFIG_DIR", ".claude") }

// defaultConfigFile is Claude Code's account file: .claude.json inside
// CLAUDE_CONFIG_DIR when set, else beside ~/.claude rather than inside it.
func defaultConfigFile() string {
	if v := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); v != "" {
		return filepath.Join(v, ".claude.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude.json")
}
