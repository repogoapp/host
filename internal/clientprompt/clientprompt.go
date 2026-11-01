// Package clientprompt selects instructions for the client's renderer.
package clientprompt

import (
	"strings"

	"github.com/repogoapp/host/internal/device"
)

// Plugins is what a session's project can do with plugins.
type Plugins int

const (
	// NoPlugins: the environment can't run plugins.
	NoPlugins Plugins = iota
	// CanAddPlugins: the environment runs plugins; the project has none yet.
	CanAddPlugins
	// HasPlugins: the project has a .repogo/plugins folder.
	HasPlugins
)

// For assembles the instructions supported by the client and project.
func For(c device.Client, plugins Plugins, servers []string) string {
	var parts []string
	for _, section := range Sections(c, plugins, servers) {
		if section.Included {
			parts = append(parts, section.Text)
		}
	}
	return strings.Join(parts, "\n")
}
