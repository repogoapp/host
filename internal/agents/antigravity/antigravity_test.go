package antigravity

import (
	"strings"
	"testing"

	"github.com/repogoapp/host/internal/agent"
	"github.com/repogoapp/host/internal/clilogin"
)

func TestTurnConfigBecomesAgysOptions(t *testing.T) {
	cases := []struct {
		cfg         agent.TurnConfig
		mode, think string
	}{
		{agent.TurnConfig{}, "default", ""},
		{agent.TurnConfig{PermissionMode: agent.PermissionAutoAcceptEdits, ReasoningLevel: "low"}, "accept-edits", "low"},
		// Extra where the model has it, else High.
		{agent.TurnConfig{PermissionMode: agent.PermissionFullAccess, ReasoningLevel: "max"}, "auto", "xhigh high"},
		// Plan mode wins over the permission mode.
		{agent.TurnConfig{Mode: "plan", PermissionMode: agent.PermissionFullAccess}, "plan", ""},
	}
	for _, c := range cases {
		got := settings(c.cfg)
		if strings.Join(got[categoryMode], " ") != c.mode || strings.Join(got[categoryEffort], " ") != c.think {
			t.Errorf("%+v: %v", c.cfg, got)
		}
	}
}

func TestRepoGoToolsRunUnasked(t *testing.T) {
	if !allows(agent.TurnConfig{}, "mcp_repogo_chats_list") {
		t.Error("RepoGo's own tool asked")
	}
	if allows(agent.TurnConfig{}, "run_command") || allows(agent.TurnConfig{}, "mcp_github_create_issue") {
		t.Error("another tool ran unasked")
	}
	if !allows(agent.TurnConfig{PermissionMode: agent.PermissionFullAccess}, "run_command") {
		t.Error("full access asked")
	}
}

func TestManifestOnlyNamesGooglesBuilds(t *testing.T) {
	sum := strings.Repeat("ab", 64)
	good := `{"version":"1.5.14","url":"https://storage.googleapis.com/antigravity-public/antigravity-cli/1.5.14-5302690847260672/darwin-arm/cli_mac_arm64.tar.gz","sha512":"` + sum + `"}`
	b, err := parseManifest([]byte(good), "darwin_arm64")
	if err != nil || b.Version != "1.5.14" {
		t.Fatalf("%+v %v", b, err)
	}
	for _, bad := range []string{
		`{"version":"1.5.14","url":"https://example.com/antigravity-cli/1.5.14-1/cli.tar.gz","sha512":"` + sum + `"}`,
		`{"version":"1.5.14","url":"https://storage.googleapis.com/antigravity-public/antigravity-cli/1.5.14-1/cli.tar.gz","sha512":"abc"}`,
		`{"version":"latest","url":"https://storage.googleapis.com/antigravity-public/antigravity-cli/latest-1/cli.tar.gz","sha512":"` + sum + `"}`,
	} {
		if _, err := parseManifest([]byte(bad), "darwin_arm64"); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := target("windows", "amd64"); err == nil {
		t.Error("windows has no package build")
	}
}

func TestSignInReadsTheGooglePage(t *testing.T) {
	var c clilogin.Code
	if loginLine("Open this link in a browser to log in with Google:", &c) {
		t.Fatal("done before the link")
	}
	if !loginLine("  https://accounts.google.com/o/oauth2/auth?client_id=x&state=y", &c) || !strings.HasPrefix(c.URL, "https://accounts.google.com/") {
		t.Fatalf("code %+v", c)
	}
}

func TestModelsListMeansSignedIn(t *testing.T) {
	if !listsModels("Fetching available models...\ngemini-3.8-flash-high\tGemini 3.8 Flash (High)\n") {
		t.Error("signed in read as out")
	}
	if listsModels("Fetching available models...\nError: Please sign in to view available models.\n") {
		t.Error("signed out read as in")
	}
}

func TestModelMakersAreNamed(t *testing.T) {
	if company("claude-opus-4-6-thinking") != "anthropic" || company("gpt-oss-120b-medium") != "openai" || company("gemini-3.8-flash") != "" {
		t.Error("wrong maker")
	}
}
