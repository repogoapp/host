package agents_test

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/repogo/host/internal/agent"
	"github.com/repogo/host/internal/agents/claude"
	"github.com/repogo/host/internal/agents/codex"
	"github.com/repogo/host/internal/clilogin"
	"github.com/repogo/host/internal/clitool"
)

func TestInstallCommandOf(t *testing.T) {
	has := func(names ...string) func(string) bool {
		return func(n string) bool {
			for _, name := range names {
				if n == name {
					return true
				}
			}
			return false
		}
	}
	cases := []struct {
		spec clitool.Spec
		has  func(string) bool
		want string
	}{
		{claude.New(agent.Dependencies{}).Definition().Tool.Spec, has(), "curl -fsSL https://claude.ai/install.sh | bash"},
		{codex.New(agent.Dependencies{}).Definition().Tool.Spec, has("npm", "brew"), "npm install -g @openai/codex"},
		{codex.New(agent.Dependencies{}).Definition().Tool.Spec, has("brew"), "brew install codex"},
		{codex.New(agent.Dependencies{}).Definition().Tool.Spec, has(), ""},
	}
	for _, c := range cases {
		if got := clitool.InstallCommandOf(c.spec, c.has); got != c.want {
			t.Errorf("clitool.InstallCommandOf(%s) = %q, want %q", c.spec.Command, got, c.want)
		}
	}
}

func TestClaudeAccount(t *testing.T) {
	dir := t.TempDir()
	path := writeFile(t, filepath.Join(dir, ".claude.json"),
		`{"oauthAccount":{"emailAddress":"me@example.com","displayName":"Me","organizationType":"max"}}`)
	if got := claude.New(agent.Dependencies{}).Definition().Tool.Spec.Account(path); got == nil || got.Email != "me@example.com" || got.Name != "Me" || got.Key != "" {
		t.Fatalf("claudeAccount = %+v", got)
	}
	// An API-key login has no oauthAccount.
	if got := claude.New(agent.Dependencies{}).Definition().Tool.Spec.Account(writeFile(t, filepath.Join(dir, "k.json"), `{"primaryApiKey":"x"}`)); got != nil {
		t.Fatalf("API key login = %+v, want nil", got)
	}
	if got := claude.New(agent.Dependencies{}).Definition().Tool.Spec.Account(filepath.Join(dir, "missing.json")); got != nil {
		t.Fatalf("missing file = %+v", got)
	}
}

func TestCodexAccount(t *testing.T) {
	dir := t.TempDir()
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"me@example.com","name":"Me"}`))
	path := writeFile(t, filepath.Join(dir, "auth.json"),
		`{"OPENAI_API_KEY":null,"tokens":{"id_token":"eyJhbGciOiJub25lIn0.`+claims+`.sig","access_token":"secret"}}`)
	if got := codex.New(agent.Dependencies{}).Definition().Tool.Spec.Account(path); got == nil || got.Email != "me@example.com" || got.Name != "Me" {
		t.Fatalf("codexAccount = %+v", got)
	}
	if got := codex.New(agent.Dependencies{}).Definition().Tool.Spec.Account(writeFile(t, filepath.Join(dir, "key.json"), `{"OPENAI_API_KEY":"sk-x"}`)); got != nil {
		t.Fatalf("API key login = %+v, want nil", got)
	}
}

// Two hosts on one seat share a key; another seat, another organization or a
// file without the ids does not.
func TestAccountKeys(t *testing.T) {
	dir := t.TempDir()
	claudeKey := func(name, user, org string) string {
		body := `{"oauthAccount":{"emailAddress":"me@example.com","accountUuid":"` + user + `","organizationUuid":"` + org + `"}}`
		a := claude.New(agent.Dependencies{}).Definition().Tool.Spec.Account(writeFile(t, filepath.Join(dir, name), body))
		if a == nil {
			t.Fatalf("%s: no account", name)
		}
		return a.Key
	}
	mac, container := claudeKey("mac.json", "u1", "o1"), claudeKey("container.json", "u1", "o1")
	if mac == "" || mac != container {
		t.Fatalf("same Claude seat: %q vs %q", mac, container)
	}
	if claudeKey("teammate.json", "u2", "o1") == mac || claudeKey("other-org.json", "u1", "o2") == mac {
		t.Fatal("another Claude seat shares the key")
	}

	codexKey := func(name, auth string) string {
		claims := base64.RawURLEncoding.EncodeToString([]byte(`{"email":"me@example.com","https://api.openai.com/auth":` + auth + `}`))
		a := codex.New(agent.Dependencies{}).Definition().Tool.Spec.Account(writeFile(t, filepath.Join(dir, name),
			`{"tokens":{"id_token":"eyJhbGciOiJub25lIn0.`+claims+`.sig"}}`))
		if a == nil {
			t.Fatalf("%s: no account", name)
		}
		return a.Key
	}
	seat := `{"chatgpt_user_id":"user-1","chatgpt_account_id":"acct-1"}`
	codex1, codex2 := codexKey("a.json", seat), codexKey("b.json", seat)
	if codex1 == "" || codex1 != codex2 {
		t.Fatalf("same Codex seat: %q vs %q", codex1, codex2)
	}
	if codexKey("teammate.json", `{"chatgpt_user_id":"user-2","chatgpt_account_id":"acct-1"}`) == codex1 {
		t.Fatal("a Codex teammate shares the key")
	}
	if codexKey("no-ids.json", `{}`) != "" {
		t.Fatal("a token without ids has a key")
	}
	// The same ids under another provider are another account.
	if clitool.AccountKey(string(agent.KindClaude), "u", "o") == clitool.AccountKey(string(agent.KindCodex), "u", "o") {
		t.Fatal("providers share a key")
	}
}

// The host's PATH was fixed when its service was installed, so a CLI the
// provider's installer just put in ~/.local/bin is found there.
func TestFindBinaryFallsBackToTheNativeInstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	if got := clitool.FindBinary(claude.New(agent.Dependencies{}).Definition().Tool.Spec, home); got != "" {
		t.Fatalf("found %q with nothing installed", got)
	}
	native := writeFile(t, filepath.Join(home, ".local/bin/claude"), "#!/bin/sh\n")
	if err := os.Chmod(native, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := clitool.FindBinary(claude.New(agent.Dependencies{}).Definition().Tool.Spec, home); got != native {
		t.Fatalf("FindBinary = %q, want %q", got, native)
	}
}

func TestCodexReleaseNamesTheTarget(t *testing.T) {
	url, member, err := codex.New(agent.Dependencies{}).Definition().Tool.Spec.Release(t.Context(), "linux", "arm64")
	if err != nil || member != "codex-aarch64-unknown-linux-musl" ||
		!strings.HasSuffix(url, "/openai/codex/releases/latest/download/codex-aarch64-unknown-linux-musl.tar.gz") {
		t.Fatalf("%s %s %v", url, member, err)
	}
	if _, member, _ := codex.New(agent.Dependencies{}).Definition().Tool.Spec.Release(t.Context(), "darwin", "amd64"); member != "codex-x86_64-apple-darwin" {
		t.Fatalf("darwin: %s", member)
	}
	if _, _, err := codex.New(agent.Dependencies{}).Definition().Tool.Spec.Release(t.Context(), "windows", "amd64"); err == nil {
		t.Fatal("windows has a release")
	}
}

// Codex 0.157 and Claude Code 2.1.282 with no terminal, captured 09-25 on a
// Debian host; the code and PKCE values are replaced.
const (
	capturedCodexLogin = "Welcome to Codex [v0.157.0]\nOpenAI's command-line coding agent\n\n" +
		"Follow these steps to sign in with ChatGPT using device code authorization:\n\n" +
		"1. Open this link in your browser and sign in to your account\n   https://auth.openai.com/codex/device\n\n" +
		"2. Enter this one-time code (expires in 15 minutes)\n   ABCD-12345\n\n" +
		"Continue only if you started this login in Codex. If a website or another person gave you this code, cancel.\n"
	capturedClaudeLogin = "Opening browser to sign in…\n" +
		"If the browser didn't open, visit: https://claude.com/cai/oauth/authorize?code=true&client_id=c&response_type=code&state=s\n" +
		"Paste code here if prompted > "
)

func readLogin(captured string, read func(string, *clilogin.Code) bool) clilogin.Code {
	var c clilogin.Code
	for _, line := range strings.Split(captured, "\n") {
		if read(strings.TrimSpace(line), &c) {
			break
		}
	}
	return c
}

func TestLoginLinksFromCapturedOutput(t *testing.T) {
	if c := readLogin(capturedCodexLogin, codex.New(agent.Dependencies{}).Definition().Tool.Spec.LoginRead); c.URL != "https://auth.openai.com/codex/device" || c.UserCode != "ABCD-12345" {
		t.Fatalf("codex = %+v", c)
	}
	c := readLogin(capturedClaudeLogin, claude.New(agent.Dependencies{}).Definition().Tool.Spec.LoginRead)
	if claude.New(agent.Dependencies{}).Definition().Tool.Spec.LoginStyle != clilogin.StylePaste || c.UserCode != "" || !strings.HasPrefix(c.URL, "https://claude.com/cai/oauth/authorize?code=true") {
		t.Fatalf("claude = %+v", c)
	}
}
