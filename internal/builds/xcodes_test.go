package builds

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

const versionPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleShortVersionString</key><string>%s</string>
<key>ProductBuildVersion</key><string>%s</string>
</dict></plist>
`

// fakeXcodes installs Xcode.app and a selected Xcode-beta.app in a fake
// /Applications, and one Spotlight finds elsewhere; it returns the beta's path.
func fakeXcodes(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("Xcode runs on macOS")
	}
	apps, elsewhere := t.TempDir(), t.TempDir()
	install := func(path, version, build string) {
		write(t, filepath.Join(path, "Contents", "version.plist"), fmt.Sprintf(versionPlist, version, build), 0o600)
	}
	install(filepath.Join(apps, "Xcode.app"), "27.0", "27A100")
	install(filepath.Join(apps, "Xcode-beta.app"), "27.1", "27B5")
	install(filepath.Join(elsewhere, "Xcode-26.app"), "26.4", "26E1")
	mkdir(t, filepath.Join(apps, "Xcode-broken.app"))
	beta := filepath.Join(apps, "Xcode-beta.app")

	tools := t.TempDir()
	write(t, filepath.Join(tools, "mdfind"), "#!/bin/sh\necho '"+filepath.Join(elsewhere, "Xcode-26.app")+
		"'\necho '/Users/me/.Trash/Xcode-old.app'\n", 0o700)
	write(t, filepath.Join(tools, "xcode-select"), "#!/bin/sh\necho '"+developerDir(beta)+"'\n", 0o700)
	oldMdfind, oldSelect, oldApps := mdfind, xcodeSelect, applications
	mdfind, xcodeSelect, applications = filepath.Join(tools, "mdfind"), filepath.Join(tools, "xcode-select"), apps
	t.Cleanup(func() { mdfind, xcodeSelect, applications = oldMdfind, oldSelect, oldApps })
	return beta
}

func TestXcodesListsInstallsSelectedFirst(t *testing.T) {
	beta := fakeXcodes(t)
	f := newFixture(t)
	xcodes, err := f.Xcodes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range xcodes {
		got = append(got, filepath.Base(x.Path)+" "+x.Version+" "+x.Build+map[bool]string{true: " selected"}[x.Selected])
	}
	// The rest sort by path, and the two temp folders' order is the system's.
	slices.Sort(got[1:])
	want := []string{"Xcode-beta.app 27.1 27B5 selected", "Xcode-26.app 26.4 26E1", "Xcode.app 27.0 27A100"}
	if xcodes[0].Path != beta || strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("xcodes = %v, want %v", got, want)
	}
}

// A build names an Xcode only from the list, so DEVELOPER_DIR never points
// at an arbitrary folder, and never on Android.
func TestBuildsRejectAnXcodeNotInstalled(t *testing.T) {
	fakeXcodes(t)
	f := newFixture(t)
	mkdir(t, filepath.Join(f.project, "Demo.xcodeproj"))
	if _, err := f.Start(Request{Project: f.project, Platform: PlatformIOS, Path: "Demo.xcodeproj", Target: "Demo",
		Xcode: "/tmp"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("build with /tmp as Xcode: %v", err)
	}
	if _, err := f.Start(Request{Project: f.project, Platform: PlatformAndroid, Xcode: "/Applications/Xcode.app"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Android build with an Xcode: %v", err)
	}
	req := publishRequest(f, "1.0.0", "")
	req.Xcode = "/tmp"
	if _, err := f.Publish(req); !errors.Is(err, ErrInvalid) {
		t.Errorf("publish with /tmp as Xcode: %v", err)
	}
}

// Every xcodebuild of a publication, the upload included, runs under the
// chosen Xcode.
func TestPublishRunsUnderTheChosenXcode(t *testing.T) {
	fakePublisher(t, "echo 'UPLOAD SUCCEEDED'")
	beta := fakeXcodes(t)
	wrapper := filepath.Join(t.TempDir(), "xcodebuild")
	write(t, wrapper, "#!/bin/sh\necho \"DEVELOPER_DIR=$DEVELOPER_DIR\" >&2\nexec '"+xcodebuild+"' \"$@\"\n", 0o700)
	xcodebuild = wrapper
	f := newFixture(t)
	mkdir(t, filepath.Join(f.project, "Demo.xcodeproj"))
	req := publishRequest(f, "1.1.0", "8")
	req.Xcode = beta
	p, err := f.Publish(req)
	if err != nil {
		t.Fatal(err)
	}
	p = waitPublication(t, f, p.ID)
	log, _ := f.PublishLog(p.ID, 0)
	if p.Status != StatusUploaded {
		t.Fatalf("publication = %+v\n%s", p, log.Data)
	}
	runs := 0
	for _, line := range strings.Split(string(log.Data), "\n") {
		if dir, ok := strings.CutPrefix(line, "DEVELOPER_DIR="); ok {
			runs++
			if dir != developerDir(beta) {
				t.Errorf("xcodebuild ran under %q, want %q", dir, developerDir(beta))
			}
		}
	}
	// Settings, archive, the IPA export and the upload.
	if runs != 4 {
		t.Fatalf("%d xcodebuild runs, want 4:\n%s", runs, log.Data)
	}
}
