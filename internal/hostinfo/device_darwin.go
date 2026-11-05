//go:build darwin

package hostinfo

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// readDevice asks the system once, at startup. system_profiler is the only
// source of the model's name and takes a moment, so it runs here, off any
// request.
func readDevice() Device {
	version, _ := unix.Sysctl("kern.osproductversion")
	name := macModelName()
	return Device{OS: "macos", OSVersion: version, FormFactor: macFormFactor(name, macHasBattery()), ModelName: name}
}

func macModelName() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/system_profiler", "-json", "SPHardwareDataType").Output()
	if err != nil {
		return ""
	}
	var report struct {
		Hardware []struct {
			MachineName string `json:"machine_name"`
		} `json:"SPHardwareDataType"`
	}
	if json.Unmarshal(out, &report) != nil || len(report.Hardware) == 0 {
		return ""
	}
	return strings.TrimSpace(report.Hardware[0].MachineName)
}

// macHasBattery is whether the Mac has a built-in battery, which only
// laptops have. Desktops list an AppleSmartBattery too, with none installed.
func macHasBattery() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-rc", "AppleSmartBattery").Output()
	return err == nil && batteryInstalled(string(out))
}
