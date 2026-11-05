package hostinfo

import (
	"bufio"
	"os"
	"strings"
)

// Device is what kind of machine the host runs on, for the phone's icon and
// the environment's details. Every field is display text or a short fixed
// list; a client that doesn't know a value falls back to a laptop.
type Device struct {
	// OS is macos, linux or windows.
	OS string `json:"os"`
	// OSVersion is the OS as people read it: "26.1", "Debian GNU/Linux 12 (bookworm)".
	OSVersion string `json:"os_version"`
	// FormFactor is laptop, desktop, server or container.
	FormFactor string `json:"form_factor"`
	// ModelName is the machine's marketing name, such as "Mac Studio", when
	// the host can read it. Display only.
	ModelName string `json:"model_name,omitempty"`
}

// The form factors a host reports.
const (
	FormLaptop    = "laptop"
	FormDesktop   = "desktop"
	FormServer    = "server"
	FormContainer = "container"
)

// macFormFactor: a MacBook, or any Mac with a built-in battery, is a laptop.
func macFormFactor(modelName string, hasBattery bool) string {
	if hasBattery || strings.HasPrefix(modelName, "MacBook") {
		return FormLaptop
	}
	return FormDesktop
}

// batteryInstalled reads ioreg's AppleSmartBattery entry, which a Mac Studio
// or mini has too, with "BatteryInstalled" = No.
func batteryInstalled(ioreg string) bool {
	return strings.Contains(ioreg, `"BatteryInstalled" = Yes`)
}

// linuxFormFactor: inside a container, then a machine with a battery, then a
// server. The host is PID 1 in a container that runs it as its only process
// (Apple's container, the sandbox), which no full Linux machine does.
func linuxFormFactor(isPID1, hasMarker, hasBattery bool) string {
	switch {
	case isPID1 || hasMarker:
		return FormContainer
	case hasBattery:
		return FormLaptop
	default:
		return FormServer
	}
}

// osRelease reads os-release's PRETTY_NAME, else NAME and VERSION_ID.
func osRelease(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fields := map[string]string{}
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		key, value, ok := strings.Cut(scan.Text(), "=")
		if ok {
			fields[key] = strings.Trim(value, `"'`)
		}
	}
	if pretty := fields["PRETTY_NAME"]; pretty != "" {
		return pretty
	}
	return strings.TrimSpace(fields["NAME"] + " " + fields["VERSION_ID"])
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
