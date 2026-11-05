package hostinfo

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStatusCarriesTheDevice(t *testing.T) {
	if got := newService(reading{}).Status().Device; got == nil || *got != studio {
		t.Fatalf("device %+v, want %+v", got, studio)
	}
}

// A device read that never finishes leaves the field out rather than holding
// host.status past deviceWait.
func TestStatusDoesNotWaitForeverOnTheDevice(t *testing.T) {
	stuck := make(chan struct{})
	defer close(stuck)
	s := New(Deps{Power: reading{}, Relay: func() bool { return false },
		ReadDevice: func() Device { <-stuck; return studio }})
	start := time.Now()
	if got := s.Status().Device; got != nil {
		t.Fatalf("device %+v before the read finished", got)
	}
	if waited := time.Since(start); waited > deviceWait+time.Second {
		t.Fatalf("status waited %v", waited)
	}
}

func TestMacFormFactor(t *testing.T) {
	for _, c := range []struct {
		name    string
		battery bool
		want    string
	}{
		{"MacBook Pro", false, FormLaptop},
		{"", true, FormLaptop},
		{"Mac Studio", false, FormDesktop},
		{"Mac mini", false, FormDesktop},
		{"", false, FormDesktop},
	} {
		if got := macFormFactor(c.name, c.battery); got != c.want {
			t.Errorf("macFormFactor(%q, %v) = %q, want %q", c.name, c.battery, got, c.want)
		}
	}
}

func TestLinuxFormFactor(t *testing.T) {
	for _, c := range []struct {
		pid1, marker, battery bool
		want                  string
	}{
		{true, false, false, FormContainer},
		{false, true, true, FormContainer},
		{false, false, true, FormLaptop},
		{false, false, false, FormServer},
	} {
		if got := linuxFormFactor(c.pid1, c.marker, c.battery); got != c.want {
			t.Errorf("linuxFormFactor(%v, %v, %v) = %q, want %q", c.pid1, c.marker, c.battery, got, c.want)
		}
	}
}

func TestOSRelease(t *testing.T) {
	dir := t.TempDir()
	pretty := filepath.Join(dir, "pretty")
	os.WriteFile(pretty, []byte("PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nNAME=\"Debian GNU/Linux\"\nVERSION_ID=\"12\"\n"), 0o644)
	plain := filepath.Join(dir, "plain")
	os.WriteFile(plain, []byte("NAME=Alpine\nVERSION_ID=3.20\n"), 0o644)
	for path, want := range map[string]string{
		pretty: "Debian GNU/Linux 12 (bookworm)", plain: "Alpine 3.20", filepath.Join(dir, "missing"): "",
	} {
		if got := osRelease(path); got != want {
			t.Errorf("osRelease(%s) = %q, want %q", filepath.Base(path), got, want)
		}
	}
}

func TestBatteryInstalled(t *testing.T) {
	studio := "+-o AppleSmartBattery  <class AppleSmartBattery>\n    {\n      \"BatteryInstalled\" = No\n"
	laptop := "+-o AppleSmartBattery  <class AppleSmartBattery>\n    {\n      \"BatteryInstalled\" = Yes\n"
	if batteryInstalled(studio) || !batteryInstalled(laptop) || batteryInstalled("") {
		t.Fatal("batteryInstalled misreads ioreg")
	}
}
