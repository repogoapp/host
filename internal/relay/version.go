package relay

import (
	"fmt"
	"os"
	"runtime/debug"
	"sync"
	"time"
)

// Version identifies the running build; the start time is what tells a stale
// binary from a fresh one.
var Version = sync.OnceValue(func() string {
	if release := os.Getenv("FLY_MACHINE_VERSION"); release != "" {
		return "fly-" + release
	}
	rev := "dev"
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if len(s.Value) >= 7 {
					rev = s.Value[:7]
				}
			case "vcs.modified":
				if s.Value == "true" {
					rev += "-dirty"
				}
			}
		}
	}
	return fmt.Sprintf("%s started %s", rev, time.Now().UTC().Format(time.RFC3339))
})
