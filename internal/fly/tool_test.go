package fly

import (
	"testing"

	"github.com/repogo/host/internal/clilogin"
)

func TestLoginLineReadsEitherWayFlyctlPrintsThePage(t *testing.T) {
	for line, want := range map[string]string{
		"Opening https://fly.io/app/auth/cli/abc123 ...":                                                        "https://fly.io/app/auth/cli/abc123",
		"failed opening browser. Copy the url (https://fly.io/app/auth/cli/abc123) into a browser and continue": "https://fly.io/app/auth/cli/abc123",
	} {
		var c clilogin.Code
		if !loginLine(line, &c) || c.URL != want {
			t.Errorf("%q: %+v", line, c)
		}
	}
	var c clilogin.Code
	if loginLine("Waiting for session...", &c) {
		t.Fatalf("read a page from %+v", c)
	}
}

func TestToolInstallsWithFlysScriptAndUpdatesItself(t *testing.T) {
	tool := Tool()
	if tool.Kind != "fly" || tool.Category != "cloud" || tool.Spec.Command != "flyctl" || tool.Spec.ReleaseRepo != "superfly/flyctl" {
		t.Fatalf("tool = %+v", tool)
	}
	if tool.Spec.InstallScript == "" || len(tool.Spec.NativeUpdate) == 0 || tool.Spec.AuthProbe == nil || !tool.Spec.LoginTTY || tool.Spec.LoginStyle != clilogin.StylePaste {
		t.Fatalf("install or sign-in is not wired: %+v", tool.Spec)
	}
}
