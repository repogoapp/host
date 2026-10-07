package builds

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/repogo/host/internal/testwait"
)

// containRoot contains to one root, the way files.Service does for real.
type containRoot string

func (c containRoot) Contain(path string) (string, error) {
	full, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if root := string(c); full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", errors.New("outside root")
	}
	return full, nil
}

type fixture struct {
	*Service
	project string
	urls    map[int]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(base, "project")
	mkdir(t, project)
	f := &fixture{project: project, urls: map[int]string{}}
	f.Service = open(t, filepath.Join(base, "state"), project, f)
	return f
}

func open(t *testing.T, dir, project string, f *fixture) *Service {
	t.Helper()
	s, err := Open(t.Context(), Config{
		Dir: dir, Paths: containRoot(project), URLs: func() map[int]string { return f.urls },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// gradleProject is a Gradle root whose wrapper runs script.
func gradleProject(t *testing.T, root, script string) {
	t.Helper()
	write(t, filepath.Join(root, "settings.gradle.kts"), `rootProject.name = "Demo"`+"\n"+`include(":app", ":lib")`, 0o600)
	write(t, filepath.Join(root, "app", "build.gradle.kts"), `plugins { id("com.android.application") }`, 0o600)
	write(t, filepath.Join(root, "lib", "build.gradle.kts"), `plugins { id("com.android.library") }`, 0o600)
	write(t, filepath.Join(root, "gradlew"), "#!/bin/sh\n"+script, 0o700)
}

// assembles is a wrapper that writes a debug APK and its metadata, as AGP does.
const assembles = `echo "gradle $@"
out=app/build/outputs/apk/debug
mkdir -p "$out"
printf 'apk-bytes' > "$out/app-debug.apk"
printf 'secret' > app/build/secret.txt
cat > "$out/output-metadata.json" <<'EOF'
{"applicationId":"com.example.demo","variantName":"debug","elements":[
 {"versionCode":7,"versionName":"1.2","outputFile":"app-debug.apk"},
 {"versionCode":7,"versionName":"1.2","outputFile":"../../../secret.txt"}]}
EOF
`

func waitDone(t *testing.T, s *Service, id string) Build {
	t.Helper()
	var b Build
	testwait.For(t, "build "+id+" to finish", func() bool {
		var err error
		b, err = s.Get(id)
		return err == nil && b.Status != StatusBuilding
	})
	return b
}

func TestAppsFindsXcodeAndGradleApps(t *testing.T) {
	f := newFixture(t)
	ios := filepath.Join(f.project, "ios")
	write(t, filepath.Join(ios, "Demo.xcodeproj", "project.pbxproj"), "PRODUCT_BUNDLE_IDENTIFIER = com.example.demo;", 0o600)
	write(t, filepath.Join(ios, "Demo.xcodeproj", "xcshareddata", "xcschemes", "Demo.xcscheme"), "", 0o600)
	write(t, filepath.Join(ios, "Demo.xcodeproj", "xcshareddata", "xcschemes", "Widgets.xcscheme"), "", 0o600)
	write(t, filepath.Join(f.project, "node_modules", "x", "Nope.xcodeproj", "project.pbxproj"), "", 0o600)
	gradleProject(t, filepath.Join(f.project, "android"), "")

	apps, err := f.Apps(f.project)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 {
		t.Fatalf("apps = %+v, want the Xcode project and the Gradle root", apps)
	}
	x, g := apps[0], apps[1]
	if x.Platform != PlatformIOS || x.Path != "ios/Demo.xcodeproj" || x.BundleID != "com.example.demo" ||
		x.DefaultTarget != "Demo" || strings.Join(x.Targets, ",") != "Demo,Widgets" || x.DefaultConfiguration != "Release" {
		t.Errorf("iOS app = %+v", x)
	}
	if g.Platform != PlatformAndroid || g.Path != "android" || g.Name != "Demo" ||
		strings.Join(g.Targets, ",") != ":app" || g.DefaultConfiguration != "debug" {
		t.Errorf("Android app = %+v; a library module is not an app", g)
	}
}

func TestAppsPrefersTheWorkspaceBesideAProject(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.project, "Demo.xcodeproj", "project.pbxproj"), "", 0o600)
	write(t, filepath.Join(f.project, "Demo.xcworkspace", "contents.xcworkspacedata"), `location = "group:Demo.xcodeproj"`, 0o600)
	apps, err := f.Apps(f.project)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].Path != "Demo.xcworkspace" {
		t.Fatalf("apps = %+v, want only the workspace", apps)
	}
}

func TestAndroidBuildKeepsOnlyTheAPKsBesideTheMetadata(t *testing.T) {
	f := newFixture(t)
	gradleProject(t, f.project, assembles)

	started, err := f.Start(Request{Project: f.project, Platform: PlatformAndroid})
	if err != nil {
		t.Fatal(err)
	}
	if started.Status != StatusBuilding || started.Target != ":app" || started.Configuration != "debug" || started.Path != "." {
		t.Fatalf("started = %+v, want defaults filled in", started)
	}
	b := waitDone(t, f.Service, started.ID)
	if b.Status != StatusSuccess {
		t.Fatalf("build = %+v\nlog: %s", b, f.LogTail(b.ID, 4096))
	}
	if b.BundleID != "com.example.demo" || b.Version != "1.2" || b.BuildNumber != "7" {
		t.Errorf("metadata = %s %s %s", b.BundleID, b.Version, b.BuildNumber)
	}
	if len(b.Artifacts) != 1 || b.Artifacts[0].Name != "app-debug.apk" || b.Artifacts[0].SizeBytes != 9 || len(b.Artifacts[0].SHA256) != 64 {
		t.Fatalf("artifacts = %+v, want only the APK", b.Artifacts)
	}
	if !strings.Contains(f.LogTail(b.ID, 4096), "gradle :app:assembleDebug --console=plain") {
		t.Errorf("log = %q, want the assemble task", f.LogTail(b.ID, 4096))
	}
}

func TestAndroidFailureIsClassifiedFromTheLog(t *testing.T) {
	f := newFixture(t)
	gradleProject(t, f.project, "echo 'SDK location not found. Define ANDROID_HOME'; exit 1\n")
	started, err := f.Start(Request{Project: f.project, Platform: PlatformAndroid})
	if err != nil {
		t.Fatal(err)
	}
	b := waitDone(t, f.Service, started.ID)
	if b.Status != StatusFailed || b.ErrorCode != "toolchain_missing" || b.ErrorMessage == "" {
		t.Fatalf("build = %+v, want a toolchain failure", b)
	}
}

func TestStartRefusesAGradleTaskInjection(t *testing.T) {
	f := newFixture(t)
	gradleProject(t, f.project, "")
	for _, req := range []Request{
		{Project: f.project, Platform: PlatformAndroid, Target: ":app; rm -rf /"},
		{Project: f.project, Platform: PlatformAndroid, Configuration: "debug --init-script=x"},
		{Project: f.project, Platform: PlatformAndroid, Path: "../elsewhere"},
		{Project: f.project, Platform: "windows"},
	} {
		if _, err := f.Start(req); err == nil {
			t.Errorf("Start(%+v) succeeded, want a refusal", req)
		}
	}
}

func TestCancelEndsTheBuildCanceled(t *testing.T) {
	f := newFixture(t)
	gradleProject(t, f.project, "echo started; exec sleep 30\n")
	started, err := f.Start(Request{Project: f.project, Platform: PlatformAndroid})
	if err != nil {
		t.Fatal(err)
	}
	testwait.For(t, "the wrapper to start", func() bool { return strings.Contains(f.LogTail(started.ID, 4096), "started") })
	if err := f.Cancel(started.ID); err != nil {
		t.Fatal(err)
	}
	if b := waitDone(t, f.Service, started.ID); b.Status != StatusCanceled {
		t.Fatalf("status = %s, want canceled", b.Status)
	}
	testwait.For(t, "the run to be released", func() bool { return f.Running() == 0 })
	if err := f.Cancel(started.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second cancel: %v, want ErrNotFound", err)
	}
}

func TestOpenSettlesABuildAStoppedHostLeftRunning(t *testing.T) {
	f := newFixture(t)
	b := Build{ID: newID(), Platform: PlatformAndroid, Project: f.project, Status: StatusBuilding, Artifacts: []Artifact{}}
	mkdir(t, f.dir(b.ID))
	if err := f.save(b); err != nil {
		t.Fatal(err)
	}
	reopened := open(t, f.cfg.Dir, f.project, f)
	got, err := reopened.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || got.ErrorCode != "host_stopped" {
		t.Fatalf("build = %+v, want failed host_stopped", got)
	}
}

func TestASuccessfulBuildPrunesItsExactDuplicatesOnly(t *testing.T) {
	f := newFixture(t)
	gradleProject(t, f.project, assembles)
	release := Build{ID: newID(), Platform: PlatformAndroid, Project: f.project, Path: ".", Target: ":app",
		Configuration: "release", Status: StatusSuccess, Artifacts: []Artifact{}, CreatedAt: 1}
	older := release
	older.ID, older.Configuration = newID(), "debug"
	for _, b := range []Build{release, older} {
		mkdir(t, f.dir(b.ID))
		if err := f.save(b); err != nil {
			t.Fatal(err)
		}
	}
	started, err := f.Start(Request{Project: f.project, Platform: PlatformAndroid})
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, f.Service, started.ID)
	testwait.For(t, "the build cleanup to finish", func() bool { return f.Running() == 0 })
	list, err := f.List(f.project)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, b := range list {
		ids = append(ids, b.ID)
	}
	if len(list) != 2 || list[0].ID != started.ID || list[1].ID != release.ID {
		t.Fatalf("builds = %v, want the new debug build and the release one", ids)
	}
}

func TestLogPagesToTheEnd(t *testing.T) {
	f := newFixture(t)
	b := Build{ID: newID(), Status: StatusSuccess, Artifacts: []Artifact{}}
	mkdir(t, f.dir(b.ID))
	if err := f.save(b); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.dir(b.ID), "build.log"), strings.Repeat("x", MaxLogRead+10), 0o600)
	first, err := f.Log(b.ID, 0)
	if err != nil || len(first.Data) != MaxLogRead || first.Done {
		t.Fatalf("first page: %d bytes, done %v, err %v", len(first.Data), first.Done, err)
	}
	second, err := f.Log(b.ID, first.NextOffset)
	if err != nil || len(second.Data) != 10 || !second.Done || second.NextOffset != MaxLogRead+10 {
		t.Fatalf("second page: %+v, err %v", second, err)
	}
	if _, err := f.Log("../../etc", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("a path as an id: %v, want ErrNotFound", err)
	}
}

func TestTokensAreBoundToABuildAndExpire(t *testing.T) {
	f := newFixture(t)
	id, other := newID(), newID()
	future := time.Now().Add(time.Hour).Unix()
	good := f.token(id, future)
	if !f.valid(id, good) {
		t.Fatal("a fresh token is refused")
	}
	for name, tok := range map[string]string{
		"another build": f.token(other, future),
		"expired":       f.token(id, time.Now().Add(-time.Minute).Unix()),
		"extended":      "9999999999." + strings.SplitN(good, ".", 2)[1],
		"empty":         "",
	} {
		if f.valid(id, tok) {
			t.Errorf("%s token accepted", name)
		}
	}
	// The secret is kept, so a link survives a restart.
	if reopened := open(t, f.cfg.Dir, f.project, f); !reopened.valid(id, good) {
		t.Error("the token died with the service")
	}
}

// servedAndroid is a finished Android build and the handler serving it.
func servedAndroid(t *testing.T) (*fixture, Build, http.Handler) {
	t.Helper()
	f := newFixture(t)
	b := Build{ID: newID(), Platform: PlatformAndroid, Project: f.project, Target: ":app", Status: StatusSuccess,
		BundleID: "com.example.demo", Artifacts: []Artifact{{Name: "app-debug.apk", SizeBytes: 10}}}
	mkdir(t, f.dir(b.ID))
	if err := f.save(b); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.dir(b.ID), "app-debug.apk"), "0123456789", 0o600)
	return f, b, f.Handler()
}

func get(h http.Handler, method, target string, header map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestInstallLinkNeedsATunnelToTheInstallPort(t *testing.T) {
	f, b, _ := servedAndroid(t)
	if err := f.Listen(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.InstallLink(b.ID); !errors.Is(err, ErrNoTunnel) {
		t.Fatalf("without a tunnel: %v, want ErrNoTunnel", err)
	}
	f.urls[f.InstallPort()] = "https://abcdefghijkl.repogo.dev"
	link, err := f.InstallLink(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(link.URL, "https://abcdefghijkl.repogo.dev/android-install/"+b.ID+"/app-debug.apk?token=") ||
		!strings.HasPrefix(link.PageURL, "https://abcdefghijkl.repogo.dev/install/"+b.ID+"?token=") || link.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("link = %+v", link)
	}

	// The listener serves what the link names.
	u, _ := url.Parse(link.URL)
	resp, err := http.Get("http://127.0.0.1:" + strconv.Itoa(f.InstallPort()) + u.RequestURI())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "0123456789" {
		t.Fatalf("GET = %d %q", resp.StatusCode, body)
	}
}

func TestHandlerServesOnlyWhatAValidTokenNames(t *testing.T) {
	f, b, h := servedAndroid(t)
	token := url.QueryEscape(f.token(b.ID, time.Now().Add(time.Hour).Unix()))
	apk := "/android-install/" + b.ID + "/app-debug.apk?token=" + token

	if w := get(h, "GET", apk, nil); w.Code != http.StatusOK || w.Body.String() != "0123456789" ||
		w.Header().Get("Content-Type") != "application/vnd.android.package-archive" {
		t.Errorf("GET = %d %q %q", w.Code, w.Body.String(), w.Header().Get("Content-Type"))
	}
	if w := get(h, "GET", apk, map[string]string{"Range": "bytes=2-4"}); w.Code != http.StatusPartialContent || w.Body.String() != "234" {
		t.Errorf("Range = %d %q", w.Code, w.Body.String())
	}
	if w := get(h, "HEAD", apk, nil); w.Code != http.StatusOK || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "10" {
		t.Errorf("HEAD = %d, %d body bytes, length %q", w.Code, w.Body.Len(), w.Header().Get("Content-Length"))
	}
	for name, target := range map[string]string{
		"no token":      "/android-install/" + b.ID + "/app-debug.apk",
		"other file":    "/android-install/" + b.ID + "/build.json?token=" + token,
		"traversal":     "/android-install/" + b.ID + "/..%2Fbuild.json?token=" + token,
		"iOS path":      "/ota/" + b.ID + "/app.ipa?token=" + token,
		"unknown build": "/android-install/" + newID() + "/app-debug.apk?token=" + token,
	} {
		if w := get(h, "GET", target, nil); w.Code == http.StatusOK {
			t.Errorf("%s: served %q", name, w.Body.String())
		}
	}
	page := get(h, "GET", "/install/"+b.ID+"?token="+token, map[string]string{"X-Forwarded-Host": "abcdefghijkl.repogo.dev"})
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `href="https://abcdefghijkl.repogo.dev/android-install/`+b.ID+`/app-debug.apk?token=`) {
		t.Errorf("page = %d\n%s", page.Code, page.Body.String())
	}
}

func TestManifestIsEscapedAndPointsAtTheTunnel(t *testing.T) {
	f := newFixture(t)
	b := Build{ID: newID(), Platform: PlatformIOS, Project: f.project, Target: `A&B <"App">`, Status: StatusSuccess,
		BundleID: "com.example.demo", BuildNumber: "42", Artifacts: []Artifact{{Name: ipaName}}}
	mkdir(t, f.dir(b.ID))
	if err := f.save(b); err != nil {
		t.Fatal(err)
	}
	token := url.QueryEscape(f.token(b.ID, time.Now().Add(time.Hour).Unix()))
	w := get(f.Handler(), "GET", "/ota/"+b.ID+"/manifest.plist?token="+token, map[string]string{"X-Forwarded-Host": "abcdefghijkl.repogo.dev"})
	body := w.Body.String()
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/xml" {
		t.Fatalf("manifest = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	for _, want := range []string{
		"<string>A&amp;B &lt;&quot;App&quot;&gt;</string>",
		"<string>https://abcdefghijkl.repogo.dev/ota/" + b.ID + "/app.ipa?token=",
		"<key>bundle-version</key><string>42</string>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("manifest lacks %q:\n%s", want, body)
		}
	}
}

// The iOS pipeline against a fake xcodebuild that writes what the real one
// does; plutil is the real one, so macOS only.
func TestIOSBuildArchivesAndExportsAnIPA(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("iOS builds run on macOS")
	}
	f := newFixture(t)
	bundle := filepath.Join(f.project, "Demo.xcodeproj")
	mkdir(t, bundle)
	fake := filepath.Join(t.TempDir(), "xcodebuild")
	write(t, fake, `#!/bin/sh
echo "xcodebuild $@" >&2
case " $* " in
*" -showBuildSettings "*)
  echo '[{"buildSettings":{"WRAPPER_EXTENSION":"app","DEVELOPMENT_TEAM":"TEAM123"}}]' ;;
*" archive "*)
  while [ "$1" != "-archivePath" ]; do shift; done
  app="$2/Products/Applications/Demo.app"
  mkdir -p "$app"
  touch "$app/embedded.mobileprovision"
  printf 'small' > "$app/AppIcon60x60@2x.png"
  printf 'larger icon' > "$app/AppIcon76x76@2x~ipad.png"
  cat > "$app/Info.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>com.example.demo</string>
<key>CFBundleShortVersionString</key><string>1.0</string>
<key>CFBundleVersion</key><string>1700000000</string>
</dict></plist>
EOF
  ;;
*" -exportArchive "*)
  while [ "$1" != "-exportPath" ]; do shift; done
  mkdir -p "$2"; printf 'ipa-bytes' > "$2/Demo.ipa"
  grep -q release-testing "$4" || exit 3 ;;
esac
`, 0o700)
	xcodebuild = fake
	t.Cleanup(func() { xcodebuild = "/usr/bin/xcodebuild" })

	started, err := f.Start(Request{Project: f.project, Platform: PlatformIOS, Path: "Demo.xcodeproj", Target: "Demo"})
	if err != nil {
		t.Fatal(err)
	}
	if started.Configuration != "Release" || started.Method != MethodAdHoc {
		t.Fatalf("started = %+v, want Release ad hoc", started)
	}
	b := waitDone(t, f.Service, started.ID)
	if b.Status != StatusSuccess {
		t.Fatalf("build = %+v\nlog: %s", b, f.LogTail(b.ID, 8192))
	}
	if b.BundleID != "com.example.demo" || b.Version != "1.0" || b.BuildNumber != "1700000000" ||
		len(b.Artifacts) != 1 || b.Artifacts[0].Name != ipaName || b.SizeBytes != 9 {
		t.Fatalf("build = %+v", b)
	}
	icon, _ := os.ReadFile(filepath.Join(f.dir(b.ID), "icon-512.png"))
	if string(icon) != "larger icon" {
		t.Errorf("icon-512 = %q, want the largest AppIcon", icon)
	}
	if _, err := os.Stat(filepath.Join(f.dir(b.ID), "app.xcarchive")); !os.IsNotExist(err) {
		t.Errorf("the archive was kept: %v", err)
	}
	if strings.Contains(f.LogTail(b.ID, 8192), "CURRENT_PROJECT_VERSION=") {
		t.Error("an ordinary build overrode the project's build number")
	}
}

func TestIOSBuildWithoutATeamFailsBeforeArchiving(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("iOS builds run on macOS")
	}
	f := newFixture(t)
	mkdir(t, filepath.Join(f.project, "Demo.xcodeproj"))
	fake := filepath.Join(t.TempDir(), "xcodebuild")
	write(t, fake, "#!/bin/sh\necho '[{\"buildSettings\":{\"WRAPPER_EXTENSION\":\"app\"}}]'\n", 0o700)
	xcodebuild = fake
	t.Cleanup(func() { xcodebuild = "/usr/bin/xcodebuild" })

	started, err := f.Start(Request{Project: f.project, Platform: PlatformIOS, Path: "Demo.xcodeproj", Target: "Demo", Configuration: "Debug"})
	if err != nil {
		t.Fatal(err)
	}
	if started.Method != MethodDevelopment {
		t.Errorf("a Debug build exports %q, want development", started.Method)
	}
	if b := waitDone(t, f.Service, started.ID); b.Status != StatusFailed || b.ErrorCode != "team_not_selected" {
		t.Fatalf("build = %+v, want team_not_selected", b)
	}
}

// servedIOS is a finished iOS build, its IPA, and a fixture that records tunnel closes.
func servedIOS(t *testing.T) (*fixture, Build, chan int) {
	t.Helper()
	f := newFixture(t)
	closed := make(chan int, 1)
	f.cfg.CloseTunnel = func(port int) error { closed <- port; return nil }
	b := Build{ID: newID(), Platform: PlatformIOS, Project: f.project, Target: "App", Status: StatusSuccess,
		BundleID: "com.example.demo", Artifacts: []Artifact{{Name: ipaName, SizeBytes: 10}}}
	mkdir(t, f.dir(b.ID))
	if err := f.save(b); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(f.dir(b.ID), ipaName), "0123456789", 0o600)
	if err := f.Listen(); err != nil {
		t.Fatal(err)
	}
	return f, b, closed
}

func TestAnIPAIsServedOnceThenTheTunnelCloses(t *testing.T) {
	defer func(d time.Duration) { closeAfter = d }(closeAfter)
	closeAfter = 0
	f, b, closed := servedIOS(t)
	token := url.QueryEscape(f.token(b.ID, time.Now().Add(TokenTTL).Unix()))
	ipa := "/ota/" + b.ID + "/" + ipaName + "?token=" + token
	h := f.Handler()

	// A HEAD and a first range leave the link working and the tunnel open.
	if w := get(h, "HEAD", ipa, nil); w.Code != http.StatusOK {
		t.Fatalf("HEAD = %d", w.Code)
	}
	if w := get(h, "GET", ipa, map[string]string{"Range": "bytes=0-4"}); w.Code != http.StatusPartialContent || w.Body.String() != "01234" {
		t.Fatalf("first range = %d %q", w.Code, w.Body.String())
	}
	select {
	case port := <-closed:
		t.Fatalf("closed port %d before the IPA was whole", port)
	default:
	}
	// The range that ends on the last byte spends the link and closes the tunnel.
	if w := get(h, "GET", ipa, map[string]string{"Range": "bytes=5-"}); w.Code != http.StatusPartialContent || w.Body.String() != "56789" {
		t.Fatalf("last range = %d %q", w.Code, w.Body.String())
	}
	select {
	case port := <-closed:
		if port != f.InstallPort() {
			t.Errorf("closed port %d, want %d", port, f.InstallPort())
		}
	case <-time.After(testwait.Timeout):
		t.Fatal("the tunnel was not closed")
	}
	if w := get(h, "GET", ipa, nil); w.Code != http.StatusGone {
		t.Errorf("second download = %d, want 410", w.Code)
	}
	// The manifest and a fresh link still work.
	if w := get(h, "GET", "/ota/"+b.ID+"/manifest.plist?token="+token, nil); w.Code != http.StatusOK {
		t.Errorf("manifest = %d", w.Code)
	}
	fresh := url.QueryEscape(f.token(b.ID, time.Now().Add(TokenTTL).Unix()+1))
	if w := get(h, "GET", "/ota/"+b.ID+"/"+ipaName+"?token="+fresh, nil); w.Code != http.StatusOK || w.Body.String() != "0123456789" {
		t.Errorf("fresh link = %d %q", w.Code, w.Body.String())
	}
}
