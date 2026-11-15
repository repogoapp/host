package screens

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestParseSimctlReadsEachRuntimesDevices(t *testing.T) {
	out := []byte(`{"devices":{
		"com.apple.CoreSimulator.SimRuntime.iOS-27-1":[{"udid":"A","name":"iPhone 17 Pro","state":"Booted"}],
		"com.apple.CoreSimulator.SimRuntime.iOS-18-2":[{"udid":"B","name":"iPhone 16","state":"Shutdown"}],
		"com.apple.CoreSimulator.SimRuntime.iOS-17-5":[]}}`)
	screens, err := parseSimctl(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []Screen{
		{Kind: KindSimulator, Platform: "ios", ID: "B", Name: "iPhone 16", OS: "iOS 18.2"},
		{Kind: KindSimulator, Platform: "ios", ID: "A", Name: "iPhone 17 Pro", OS: "iOS 27.1", Booted: true},
	}
	if len(screens) != len(want) {
		t.Fatalf("screens = %+v", screens)
	}
	for i := range want {
		if screens[i] != want[i] {
			t.Fatalf("screens[%d] = %+v, want %+v", i, screens[i], want[i])
		}
	}
}

func TestEmulatorSerialsSkipsPhonesAndOfflineEmulators(t *testing.T) {
	out := "List of devices attached\nemulator-5554\tdevice\nemulator-5556\toffline\nR58M12345\tdevice\n"
	got := emulatorSerials(out)
	if len(got) != 1 || got[0] != "emulator-5554" {
		t.Fatalf("serials = %v", got)
	}
}

func TestAVDAPILevelReadsTheIniTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ANDROID_AVD_HOME", home)
	os.WriteFile(filepath.Join(home, "Pixel_9.ini"), []byte("avd.ini.encoding=UTF-8\npath=/x/Pixel_9.avd\ntarget=android-37.0\n"), 0o644)
	if got := avdAPILevel("Pixel_9"); got != "API 37.0" {
		t.Fatalf("API level = %q", got)
	}
}

// Lists the real machine's screens; run with -v to see them.
func TestListOnThisMachine(t *testing.T) {
	if testing.Short() {
		t.Skip("reads the machine's tools")
	}
	screens, err := List(context.Background())
	out, _ := json.MarshalIndent(screens, "", "  ")
	t.Logf("%s", out)
	if err != nil {
		t.Log(err)
	}
}
