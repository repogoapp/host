//go:build windows

package ports

import "context"

// Windows port enumeration is not written yet; an empty list still forwards a
// typed port.
func listPlatformPorts(context.Context) []Port { return nil }
