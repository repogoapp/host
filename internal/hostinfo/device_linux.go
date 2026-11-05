//go:build linux

package hostinfo

import (
	"os"
	"path/filepath"
	"strings"
)

func readDevice() Device {
	product, _ := os.ReadFile("/sys/class/dmi/id/product_name")
	batteries, _ := filepath.Glob("/sys/class/power_supply/BAT*")
	_, inContainer := os.LookupEnv("container")
	marker := inContainer || exists("/.dockerenv") || exists("/run/.containerenv")
	return Device{
		OS:         "linux",
		OSVersion:  osRelease("/etc/os-release"),
		FormFactor: linuxFormFactor(os.Getpid() == 1, marker, len(batteries) > 0),
		ModelName:  strings.TrimSpace(string(product)),
	}
}
