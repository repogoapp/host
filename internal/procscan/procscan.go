// Package procscan reads other processes: their working directories, so a port
// is attributed to its project (`ps` and lsof, as pgrep omits hardened
// binaries on macOS), and start times, so a pid is known across a restart.
package procscan

import "time"

const scanTimeout = 3 * time.Second
