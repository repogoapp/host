package tunnelwire

import (
	"regexp"
	"strings"
)

// EnvPrefix starts an environment's own name, h-<host id>. Tunnel slugs
// never hold a hyphen, so the two kinds of name can't meet.
const EnvPrefix = "h-"

// hostID is a host's id as the gateway derives it from its key: 32 hex digits.
var hostID = regexp.MustCompile(`^[0-9a-f]{32}$`)

var (
	pluginName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	methodName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
)

// EnvLabel is the DNS label of the host's environment address.
func EnvLabel(id string) string { return EnvPrefix + id }

// EnvHostID is the host id in an environment label, if label is one.
func EnvHostID(label string) (string, bool) {
	id, ok := strings.CutPrefix(label, EnvPrefix)
	return id, ok && hostID.MatchString(id)
}

// PluginRoute is what a PluginRequest's path names: /<plugin>/<method>, then
// Rest, the rest of the path, for the method to read.
type PluginRoute struct {
	Plugin string
	Method string
	Rest   string
}

// ParsePluginPath reads a request path, without its query. The gateway and
// the host both check it, so a path either refuses never reaches a plugin.
func ParsePluginPath(path string) (PluginRoute, bool) {
	parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 3)
	if !strings.HasPrefix(path, "/") || len(parts) < 2 || !pluginName.MatchString(parts[0]) || !methodName.MatchString(parts[1]) {
		return PluginRoute{}, false
	}
	route := PluginRoute{Plugin: parts[0], Method: parts[1]}
	if len(parts) == 3 {
		route.Rest = parts[2]
	}
	return route, true
}
