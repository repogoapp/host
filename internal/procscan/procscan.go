// Package procscan reads processes' working directories, so a listening port
// can be attributed to the project it serves. `ps` and lsof rather than pgrep,
// which omits hardened binaries on macOS.
package procscan

import "time"

const scanTimeout = 3 * time.Second
