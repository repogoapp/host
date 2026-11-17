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
		"com.apple.CoreSimulator.SimRuntime.iOS-9-3":[{"udid":"C","name":"iPhone 17 Pro","state":"Shutdown"}],
		"com.apple.CoreSimulator.SimRuntime.iOS-18-2":[{"udid":"B","name":"iPad Air","state":"Shutdown"}],
		"com.apple.CoreSimulator.SimRuntime.watchOS-12-0":[{"udid":"W","name":"Apple Watch","state":"Shutdown"}],
		"com.apple.CoreSimulator.SimRuntime.iOS-17-5":[]}}`)
	screens, err := parseSimctl(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []Screen{
		{Kind: KindSimulator, Platform: PlatformIOS, ID: "B", Name: "iPad Air", OS: OS{Name: "iOS", Version: "18.2"}, State: StateShutdown, Form: FormTablet},
		{Kind: KindSimulator, Platform: PlatformIOS, ID: "A", Name: "iPhone 17 Pro", OS: OS{Name: "iOS", Version: "27.1"}, State: StateBooted, Form: FormPhone},
		{Kind: KindSimulator, Platform: PlatformIOS, ID: "C", Name: "iPhone 17 Pro", OS: OS{Name: "iOS", Version: "9.3"}, State: StateShutdown, Form: FormPhone},
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

func TestAVDOSReadsTheIniTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("ANDROID_AVD_HOME", home)
	os.WriteFile(filepath.Join(home, "Pixel_9.ini"), []byte("avd.ini.encoding=UTF-8\npath=/x/Pixel_9.avd\ntarget=android-36\n"), 0o644)
	got := avdOS("Pixel_9")
	if got.Name != "Android" || got.Version != "16" || got.APILevel == nil || *got.APILevel != 36 {
		t.Fatalf("OS = %+v", got)
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
