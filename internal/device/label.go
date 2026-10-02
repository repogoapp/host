package device

import (
	"os"
	"strings"
)

// LabelEnv names this machine where its hostname means nothing to the user,
// such as a cloud sandbox, which its provisioner names after the environment.
const LabelEnv = "REPOGO_HOST_LABEL"

// Label is what a client calls this machine: the hostname, one syscall and
// what every other tool calls it, minus the `.local` mDNS suffix. Falls back to
// a constant because a machine that cannot name itself should still pair.
func Label() string {
	if name := strings.TrimSpace(os.Getenv(LabelEnv)); name != "" {
		return name
	}
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "Environment"
	}
	return strings.TrimSuffix(name, ".local")
}
