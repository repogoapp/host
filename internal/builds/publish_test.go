package builds

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/repogo/host/internal/testwait"
)

func fakePublisher(t *testing.T, upload string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("iOS publishing runs on macOS")
	}
	fake := filepath.Join(t.TempDir(), "xcodebuild")
	write(t, fake, `#!/bin/sh
set -eu
echo "xcodebuild $*"
case " $* " in
*" -showBuildSettings "*)
  echo '[{"buildSettings":{"WRAPPER_EXTENSION":"appex","MARKETING_VERSION":"9.9"}},{"buildSettings":{"WRAPPER_EXTENSION":"app","DEVELOPMENT_TEAM":"TEAM123","MARKETING_VERSION":"1.0.3","CURRENT_PROJECT_VERSION":"44"}}]'
  exit 0 ;;
*" archive "*)
  version=1.0.0
  build=7
  archive=''
  while [ "$#" -gt 0 ]; do
    case "$1" in
      -archivePath) archive="$2"; shift ;;
      MARKETING_VERSION=*) version="${1#*=}" ;;
      CURRENT_PROJECT_VERSION=*) build="${1#*=}" ;;
    esac
    shift
  done
  app="$archive/Products/Applications/Demo.app"
  mkdir -p "$app"
  touch "$app/embedded.mobileprovision"
  cat > "$app/Info.plist" <<EOF
<?xml version="1.0"?><plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>com.example.demo</string>
<key>CFBundleShortVersionString</key><string>$version</string>
<key>CFBundleVersion</key><string>$build</string>
</dict></plist>
EOF
  exit 0 ;;
esac
out=''
options=''
while [ "$#" -gt 0 ]; do
  case "$1" in
    -exportPath) out="$2"; shift ;;
    -exportOptionsPlist) options="$2"; shift ;;
  esac
  shift
done
mkdir -p "$out"
if grep -q app-store-connect "$options"; then
  grep -q '<key>destination</key><string>upload</string>' "$options"
  if grep -q '<key>manageAppVersionAndBuildNumber</key><true/>' "$options"; then
    echo 'managed build number'
    cat > "$out/DistributionSummary.plist" <<EOF
<?xml version="1.0"?><plist version="1.0"><dict><key>Demo.ipa</key><array><dict>
<key>buildNumber</key><string>45</string><key>versionNumber</key><string>1.0.3</string>
</dict></array></dict></plist>
EOF
  fi
  `+upload+`
else
  printf 'ipa' > "$out/Demo.ipa"
fi
`, 0o700)
	old := xcodebuild
	xcodebuild = fake
	t.Cleanup(func() { xcodebuild = old })
}

func publishRequest(f *fixture, version, build string) PublishRequest {
	return PublishRequest{RequestID: newID(), Project: f.project, Path: "Demo.xcodeproj", Target: "Demo",
		Version: version, BuildNumber: build}
}

func waitPublication(t *testing.T, f *fixture, id string) Publication {
	t.Helper()
	var p Publication
	testwait.For(t, "publication to finish", func() bool {
		var err error
		p, err = f.Publication(id)
		return err == nil && p.Status != StatusPublishing && f.Running() == 0
	})
	return p
}

func TestPublishBuildsThenUploads(t *testing.T) {
	fakePublisher(t, "echo 'UPLOAD SUCCEEDED'")
	f := newFixture(t)
	mkdir(t, filepath.Join(f.project, "Demo.xcodeproj"))
	req := publishRequest(f, "1.1.0", "8")
	p, err := f.Publish(req)
	if err != nil {
		t.Fatal(err)
	}
	p = waitPublication(t, f, p.ID)
	log, _ := f.PublishLog(p.ID, 0)
	if p.Status != StatusUploaded || p.BundleID != "com.example.demo" {
		t.Fatalf("publication = %+v\n%s", p, log.Data)
	}
	for _, want := range []string{"MARKETING_VERSION=1.1.0", "CURRENT_PROJECT_VERSION=8", "UPLOAD SUCCEEDED"} {
		if !strings.Contains(string(log.Data), want) {
			t.Errorf("the publish log is missing %q:\n%s", want, log.Data)
		}
	}
	built, err := f.Get(p.BuildID)
	if err != nil || built.Configuration != "Release" || built.Version != "1.1.0" || built.BuildNumber != "8" {
		t.Fatalf("build = %+v, %v", built, err)
	}
	if _, err := os.Stat(filepath.Join(f.dir(built.ID), "app.xcarchive")); !os.IsNotExist(err) {
		t.Errorf("the archive outlived its upload: %v", err)
	}

	if again, err := f.Publish(req); err != nil || again.ID != p.ID {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	req.BuildNumber = "9"
	if _, err := f.Publish(req); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reused request id with different numbers: %v", err)
	}
	rows, err := f.Publications(f.project, "Demo.xcodeproj", "Demo")
	if err != nil || len(rows) != 1 || rows[0].ID != p.ID {
		t.Fatalf("list = %+v, %v", rows, err)
	}
	if rows, _ := f.Publications(f.project, "Demo.xcodeproj", "Widgets"); len(rows) != 0 {
		t.Fatalf("another target's list = %+v", rows)
	}
}

func TestPublishLetsXcodePickTheBuildNumber(t *testing.T) {
	fakePublisher(t, "echo 'UPLOAD SUCCEEDED'")
	f := newFixture(t)
	mkdir(t, filepath.Join(f.project, "Demo.xcodeproj"))
	req := publishRequest(f, "1.0.3", "")
	p, err := f.Publish(req)
	if err != nil {
		t.Fatal(err)
	}
	p = waitPublication(t, f, p.ID)
	log, _ := f.PublishLog(p.ID, 0)
	if p.Status != StatusUploaded || p.BuildNumber != "45" {
		t.Fatalf("publication = %+v\n%s", p, log.Data)
	}
	if !strings.Contains(string(log.Data), "managed build number") || strings.Contains(string(log.Data), "CURRENT_PROJECT_VERSION=") {
		t.Fatalf("the upload did not leave the build number to Xcode:\n%s", log.Data)
	}
	if again, err := f.Publish(req); err != nil || again.ID != p.ID {
		t.Fatalf("replay after Xcode filled the build number = %+v, %v", again, err)
	}
}

func TestPublishRejectsMalformedNumbers(t *testing.T) {
	if !IOSSupported() {
		t.Skip("iOS publishing runs on macOS")
	}
	f := newFixture(t)
	for _, numbers := range [][2]string{{"", "7"}, {"1..0", "8"}, {"1.0", "$(touch x)"}, {"1.2.3.4", ""}} {
		if _, err := f.Publish(publishRequest(f, numbers[0], numbers[1])); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted %q (%q): %v", numbers[0], numbers[1], err)
		}
	}
}

func TestPublishReportsAppStoreConnectRefusal(t *testing.T) {
	fakePublisher(t, `echo "ERROR: Redundant Binary Upload. You've already uploaded a build with build number '8' for version number '1.1.0'."; exit 70`)
	f := newFixture(t)
	mkdir(t, filepath.Join(f.project, "Demo.xcodeproj"))
	p, err := f.Publish(publishRequest(f, "1.1.0", "8"))
	if err != nil {
		t.Fatal(err)
	}
	if p := waitPublication(t, f, p.ID); p.Status != StatusFailed || p.ErrorCode != "build_number_used" {
		t.Fatalf("publication = %+v", p)
	}
}

func TestProjectNumbersReadTheAppTarget(t *testing.T) {
	fakePublisher(t, "")
	f := newFixture(t)
	mkdir(t, filepath.Join(f.project, "Demo.xcodeproj"))
	got, err := f.ProjectNumbers(t.Context(), f.project, "Demo.xcodeproj", "Demo")
	if err != nil || got != (Numbers{Version: "1.0.3", BuildNumber: "44"}) {
		t.Fatalf("numbers = %+v, %v", got, err)
	}
	write(t, filepath.Join(f.project, "Demo.txt"), "", 0o600)
	if _, err := f.ProjectNumbers(t.Context(), f.project, "Demo.txt", "Demo"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("accepted a path that isn't a project: %v", err)
	}
}

func TestPublishDoesNotGuessAfterUploadError(t *testing.T) {
	fakePublisher(t, "echo 'connection lost'; exit 1")
	f := newFixture(t)
	mkdir(t, filepath.Join(f.project, "Demo.xcodeproj"))
	p, err := f.Publish(publishRequest(f, "1.0.0", "7"))
	if err != nil {
		t.Fatal(err)
	}
	p = waitPublication(t, f, p.ID)
	if p.Status != StatusUnknown || p.ErrorCode != "upload_uncertain" {
		t.Fatalf("publication = %+v", p)
	}
}

func TestPublishPinsBuildAndCancelsUpload(t *testing.T) {
	fakePublisher(t, "echo 'upload running'; while :; do :; done")
	f := newFixture(t)
	mkdir(t, filepath.Join(f.project, "Demo.xcodeproj"))
	p, err := f.Publish(publishRequest(f, "1.0.0", "7"))
	if err != nil {
		t.Fatal(err)
	}
	testwait.For(t, "fake upload to start", func() bool {
		log, _ := f.PublishLog(p.ID, 0)
		return strings.Contains(string(log.Data), "upload running")
	})
	if _, err := f.Publish(publishRequest(f, "1.0.1", "1")); !errors.Is(err, ErrNotReady) {
		t.Fatalf("started a second upload of the app: %v", err)
	}
	p, _ = f.Publication(p.ID)
	if err := f.Delete(p.BuildID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("deleted the uploading build: %v", err)
	}
	if err := f.CancelPublish(p.ID); err != nil {
		t.Fatal(err)
	}
	if p := waitPublication(t, f, p.ID); p.Status != StatusUnknown {
		t.Fatalf("a canceled upload must not claim delivery failed: %+v", p)
	}
}

func TestPublicationRecoveryAndLogPaging(t *testing.T) {
	f := newFixture(t)
	for _, phase := range []string{"build", "upload"} {
		p := Publication{ID: newID(), Project: f.project, Status: StatusPublishing, Phase: phase}
		write(t, filepath.Join(f.publicationDir(p.ID), "publish.log"), strings.Repeat("x", MaxLogRead+3), 0o600)
		if err := f.savePublication(p); err != nil {
			t.Fatal(err)
		}
		if err := f.settlePublications(); err != nil {
			t.Fatal(err)
		}
		got, err := f.Publication(p.ID)
		want := StatusFailed
		if phase == "upload" {
			want = StatusUnknown
		}
		if err != nil || got.Status != want || got.FinishedAt == 0 {
			t.Fatalf("recovery = %+v, %v", got, err)
		}
		first, err := f.PublishLog(p.ID, 0)
		if err != nil || first.Done || first.NextOffset != MaxLogRead {
			t.Fatalf("first log page = %+v, %v", first, err)
		}
		last, err := f.PublishLog(p.ID, first.NextOffset)
		if err != nil || !last.Done || string(last.Data) != "xxx" {
			t.Fatalf("last log page = %+v, %v", last, err)
		}
	}
}

func TestBuildOverridesRejectInvalidValues(t *testing.T) {
	for _, req := range []Request{
		{Platform: PlatformAndroid, Version: "1.0.0"},
		{Platform: PlatformIOS, Version: "1.2.3.4"},
		{Platform: PlatformIOS, BuildNumber: "$(touch file)"},
	} {
		if err := validateOverrides(req); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted %+v: %v", req, err)
		}
	}
}
