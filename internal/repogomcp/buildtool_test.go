package repogomcp

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// An agent builds the turn's project with no more than a platform, and the
// status it waits on says what the user must do for a link.
func TestBuildToolBuildsTheTurnsProject(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(base, "project")
	for path, body := range map[string]string{
		"settings.gradle":  `include ':app'`,
		"app/build.gradle": `apply plugin: 'com.android.application'`,
		"gradlew": `#!/bin/sh
out=app/build/outputs/apk/debug; mkdir -p $out; printf apk > $out/app-debug.apk
echo '{"applicationId":"com.example","variantName":"debug","elements":[{"versionCode":1,"versionName":"1.0","outputFile":"app-debug.apk"}]}' > $out/output-metadata.json
`,
	} {
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
	token, _ := bearer(t, s, &fakeSession{turn: turn})

	if got := callTool(t, s, token, "build", map[string]any{"platform": "ios"}); !got.IsError || !strings.Contains(got.Content[0].Text, "no ios app") {
		t.Errorf("an iOS build of an Android project: %+v", got)
	}
	started := callTool(t, s, token, "build", map[string]any{"platform": "android"})
	var b builds.Build
	if started.IsError || json.Unmarshal([]byte(started.Content[0].Text), &b) != nil || b.Status != builds.StatusBuilding || b.Target != ":app" {
		t.Fatalf("build: %+v", started)
	}
	status := callTool(t, s, token, "build_status", map[string]any{"build_id": b.ID})
	var got buildStatus
	if status.IsError || json.Unmarshal([]byte(status.Content[0].Text), &got) != nil {
		t.Fatalf("build_status: %+v", status)
	}
	if got.Build.Status != builds.StatusSuccess || got.Install != nil || !strings.Contains(got.InstallError, "open a tunnel") {
		t.Fatalf("status = %+v, want success and a tunnel to open", got)
	}
}
