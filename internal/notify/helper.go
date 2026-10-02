package notify

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/repogo/host/internal/apphome"
)

// helperVersion is bumped whenever helperSource changes so an install refreshes
// a stale script rather than leaving an old one wired up.
const helperVersion = 7

// helperSource is POSIX sh so it runs without this binary or Node. Claude
// passes the payload on stdin, Codex as the last argv; the drop directory is
// baked in so a host under REPOGO_HOME hears its own hooks.
const helperSource = `#!/bin/sh
# repogo-notify (v%d) — writes an agent hook payload into the drop directory.
# Installed by RepoGo. Safe to delete; hooks then simply stop reporting.
set -e

AGENT="$1"
EVENT="$2"
DIR=%s
# Set on agents the host spawns for a device; a terminal never has it.
ORIGIN="${REPOGO_ORIGIN:-cli}"

if [ -n "$3" ]; then
  PAYLOAD="$3"
else
  PAYLOAD=$(cat)
fi
[ -n "$PAYLOAD" ] || PAYLOAD='{}'

# One clock read, to the millisecond where date has %%N (GNU, macOS 14+);
# whole seconds would time a turn up to a second long.
NOW=$(date +%%s.%%N)
case "$NOW" in
  *[!0-9.]* | *. | .*) MS="$(date +%%s)000" ;;
  *) MS="${NOW%%.*}$(printf '%%.3s' "${NOW#*.}")" ;;
esac

mkdir -p "$DIR"
TMP=$(mktemp "$DIR/drop.XXXXXXXX") || exit 0

printf '{"agent":"%%s","event":"%%s","origin":"%%s","at_ms":%%s,"payload":%%s}' \
  "$AGENT" "$EVENT" "$ORIGIN" "$MS" "$PAYLOAD" > "$TMP"

# Rename last so the watcher never reads a half-written drop. The timestamp
# prefix keeps replay in the order the hooks actually fired.
mv "$TMP" "$DIR/$MS-$$.json"
`

// DefaultDir is where drops land, under the host's own state directory.
func DefaultDir() (string, error) {
	return apphome.Path("notify")
}

// HelperPath is where the hook script is installed. The full path goes into
// the user's agent config, so it sits under ~/.repogo.
func HelperPath() (string, error) {
	return apphome.Path("hooks", "repogo-notify")
}

// WriteHelper installs the helper script, overwriting an older version.
func WriteHelper() (string, error) {
	path, err := HelperPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	dir, err := DefaultDir()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(helperScript(dir)), 0o700); err != nil {
		return "", err
	}
	return path, nil
}

// helperScript is the helper writing drops into dir.
func helperScript(dir string) string {
	quoted := "'" + strings.ReplaceAll(dir, "'", `'\''`) + "'"
	return fmt.Sprintf(helperSource, helperVersion, quoted)
}

// AgentStatus reports whether our hook is wired into one agent, and whether
// doing so would disturb something already there.
type AgentStatus struct {
	Agent      string `json:"agent"`
	ConfigPath string `json:"config_path"`
	Installed  bool   `json:"installed"`

	// Set when installing would overwrite a hook we do not own. Nothing is
	// changed in that case — clobbering another tool's integration to enable
	// ours is not a trade we get to make on the user's behalf.
	Conflict string `json:"conflict,omitempty"`
}
