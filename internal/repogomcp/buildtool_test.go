package repogomcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/builds"
)

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

// buildServer serves the tools for a turn in a project holding files.
func buildServer(t *testing.T, files map[string]string, ask func(context.Context, agent.Approval) (json.RawMessage, error)) (*Server, string, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(base, "project")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, body := range files {
		full := filepath.Join(project, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	svc, err := builds.Open(t.Context(), builds.Config{
		Dir: filepath.Join(base, "builds"), Paths: containRoot(project),
		URLs: func() map[int]string { return nil }, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Close)
	s := New(Config{
		URL: func() string { return "http://127.0.0.1:1/mcp/repogo" }, On: func(string) bool { return true },
		Browser: newFakePhone(true, nil).browser, Builds: func() *builds.Service { return svc },
	})
	turn := turnFrom("phone")
	turn.Cwd = project
	turn.Ask = ask
	token, _ := bearer(t, s, &fakeSession{turn: turn})
	return s, token, project
}

// An agent builds the turn's project with no more than a platform, and the
// status it waits on says what the user must do for a link.
func TestBuildToolBuildsTheTurnsProject(t *testing.T) {
	s, token, _ := buildServer(t, map[string]string{
		"settings.gradle":  `include ':app'`,
		"app/build.gradle": `apply plugin: 'com.android.application'`,
		"gradlew": `#!/bin/sh
out=app/build/outputs/apk/debug; mkdir -p $out; printf apk > $out/app-debug.apk
echo '{"applicationId":"com.example","variantName":"debug","elements":[{"versionCode":1,"versionName":"1.0","outputFile":"app-debug.apk"}]}' > $out/output-metadata.json
`,
	}, nil)

	if got := callTool(t, s, token, "build", map[string]any{"platform": "ios"}); !got.IsError || !strings.Contains(got.Content[0].Text, "no ios app") {
		t.Errorf("an iOS build of an Android project: %+v", got)
	}
	apps := callTool(t, s, token, "apps", map[string]any{})
	var listed appsResult
	if apps.IsError || json.Unmarshal([]byte(apps.Content[0].Text), &listed) != nil ||
		len(listed.Apps) != 1 || listed.Apps[0].DefaultTarget != ":app" || listed.Xcodes == nil {
		t.Fatalf("apps: %+v", apps)
	}
	started := callTool(t, s, token, "build", map[string]any{"platform": "android"})
	var b builds.Build
	if started.IsError || json.Unmarshal([]byte(started.Content[0].Text), &b) != nil || b.Status != builds.StatusBuilding || b.Target != ":app" {
		t.Fatalf("build: %+v", started)
	}
	status := callTool(t, s, token, "status", map[string]any{"id": b.ID})
	var got jobStatus
	if status.IsError || json.Unmarshal([]byte(status.Content[0].Text), &got) != nil || got.Kind != "build" || got.Build == nil {
		t.Fatalf("status: %+v", status)
	}
	if got.Build.Status != builds.StatusSuccess || got.Install != nil || !strings.Contains(got.InstallError, "open a tunnel") {
		t.Fatalf("status = %+v, want success and a tunnel to open", got)
	}
	if got := callTool(t, s, token, "cancel", map[string]any{"id": b.ID}); !got.IsError || !strings.Contains(got.Content[0].Text, "is running") {
		t.Errorf("cancel of a finished build: %+v", got)
	}
}

// Nothing leaves the machine until the user taps Upload on the approval the
// tool raises; a decline or a turn that cannot ask uploads nothing.
func TestPublishUploadsOnlyOnceTheUserApproves(t *testing.T) {
	if !builds.IOSSupported() {
		t.Skip("iOS publishing runs on macOS")
	}
	args := map[string]any{"path": "Demo.txt", "target": "Demo", "version": "1.2.0"}
	var asked []agent.Approval
	answer := `"reject"`
	ask := func(_ context.Context, a agent.Approval) (json.RawMessage, error) {
		asked = append(asked, a)
		return json.RawMessage(answer), nil
	}
	s, token, _ := buildServer(t, nil, ask)

	if got := callTool(t, s, token, "publish", map[string]any{"path": "Demo.txt"}); !got.IsError || len(asked) != 0 {
		t.Fatalf("publish without a target and version: %+v, asked %d", got, len(asked))
	}
	if got := callTool(t, s, token, "publish", args); !got.IsError || !strings.Contains(got.Content[0].Text, "declined") {
		t.Fatalf("declined publish: %+v", got)
	}
	a := asked[0]
	if !strings.HasPrefix(a.CallID, "publish:") || a.Title != "Upload Demo 1.2.0 to App Store Connect" ||
		len(a.Options) != 2 || a.Options[0].Kind != "allow_once" || !strings.HasPrefix(a.Options[1].Kind, "reject") {
		t.Fatalf("approval = %+v", a)
	}

	answer = `"upload"`
	started := callTool(t, s, token, "publish", args)
	var p builds.Publication
	if started.IsError || json.Unmarshal([]byte(started.Content[0].Text), &p) != nil || p.Status != builds.StatusPublishing {
		t.Fatalf("approved publish: %+v", started)
	}
	if "publish:"+p.ID != asked[1].CallID {
		t.Errorf("the publication %s is not the one approved (%s)", p.ID, asked[1].CallID)
	}
	// Demo.txt is no Xcode project, so the build fails before xcodebuild runs.
	status := callTool(t, s, token, "status", map[string]any{"id": p.ID})
	var got jobStatus
	if status.IsError || json.Unmarshal([]byte(status.Content[0].Text), &got) != nil ||
		got.Kind != "publish" || got.Publication == nil || got.Publication.Status != builds.StatusFailed {
		t.Fatalf("status: %+v", status)
	}
}

func TestPublishNeedsATurnThatCanAsk(t *testing.T) {
	if !builds.IOSSupported() {
		t.Skip("iOS publishing runs on macOS")
	}
	s, token, _ := buildServer(t, nil, nil)
	got := callTool(t, s, token, "publish", map[string]any{"path": "Demo.xcodeproj", "target": "Demo", "version": "1.0"})
	if !got.IsError || !strings.Contains(got.Content[0].Text, "nothing was uploaded") {
		t.Fatalf("publish from a turn that cannot ask: %+v", got)
	}
}

// Switching the server off stops the turn already running: its session still
// holds a bearer until the next turn restarts it without the server.
func TestToolsRefuseOnceSwitchedOff(t *testing.T) {
	asked := 0
	s, token, _ := buildServer(t, nil, func(context.Context, agent.Approval) (json.RawMessage, error) {
		asked++
		return json.RawMessage(`"upload"`), nil
	})
	s.cfg.On = func(string) bool { return false }
	for _, name := range []string{"apps", "build", "publish", "browser"} {
		got := callTool(t, s, token, name, map[string]any{"platform": "ios", "path": "Demo.xcodeproj", "target": "Demo", "version": "1.0"})
		if !got.IsError || !strings.Contains(got.Content[0].Text, "switched off") {
			t.Errorf("%s while switched off: %+v", name, got)
		}
	}
	if asked != 0 {
		t.Errorf("asked the user %d times while switched off", asked)
	}
}
