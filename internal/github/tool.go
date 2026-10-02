package github

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/repogo/host/internal/binfetch"
	"github.com/repogo/host/internal/clilogin"
	"github.com/repogo/host/internal/clitool"
)

const deviceURL = "https://github.com/login/device"

var (
	// gh 2.57 prints "First copy your one-time code: XXXX-XXXX"; 2.101
	// prints "One-time code (XXXX-XXXX) copied to clipboard".
	codeLine = regexp.MustCompile(`(?i:one-time code)\W+([A-Z0-9]{4}-[A-Z0-9]{4})`)
	urlLine  = regexp.MustCompile(`https://\S+/login/device`)
)

// Tool is gh: installed with Homebrew or its release build, signed in with
// its browser sign-in, which with no terminal prints a device code.
func (s *Service) Tool() clitool.Tool {
	return clitool.Tool{
		Kind:         "github",
		Name:         "GitHub",
		Icon:         "github",
		Category:     clitool.CategorySourceControl,
		Capabilities: []string{clitool.CapabilityInstall, clitool.CapabilityUpdate, clitool.CapabilityLogin},
		Spec: clitool.Spec{
			Command:     "gh",
			Formula:     "gh",
			ReleaseRepo: "cli/cli",
			Release:     ghRelease,
			NativePaths: []string{".local/bin/gh"},
			AuthProbe:   signedIn,
			LoginArgs:   []string{"auth", "login", "--web", "--hostname", "github.com", "--git-protocol", "https"},
			LoginEnv:    []string{"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1"},
			LoginRead:   readDeviceLine,
			AfterSignIn: s.setupGit,
			LogoutArgs:  []string{"auth", "logout", "--hostname", "github.com"},
		},
	}
}

// signedIn asks GitHub who gh is signed in as; `gh api user` rather than
// `gh auth status`, whose output is prose.
func signedIn(ctx context.Context, path string) (bool, string, *clitool.Account) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "api", "user", "--jq", `[.login, .avatar_url] | @tsv`)
	cmd.Env = append(cmd.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	out, err := cmd.Output()
	if err != nil {
		return false, "", nil
	}
	login, avatar, _ := strings.Cut(strings.TrimSpace(string(out)), "\t")
	if login == "" {
		return false, "", nil
	}
	return true, "gh api user", &clitool.Account{Name: login, Login: login, AvatarURL: avatar}
}

// setupGit points git at gh's token, which is what makes HTTPS pushes work.
func (s *Service) setupGit(ctx context.Context, path string) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, path, "auth", "setup-git").CombinedOutput(); err != nil {
		s.log.Warn("github: gh auth setup-git failed; HTTPS pushes may prompt", "err", err, "output", strings.TrimSpace(string(out)))
	}
}

// readDeviceLine reads gh's output up to the code. The page is always
// github.com's for this hostname; a URL printed first still wins.
func readDeviceLine(line string, c *clilogin.Code) bool {
	if c.URL == "" {
		c.URL = deviceURL
	}
	if u := urlLine.FindString(line); u != "" {
		c.URL = u
	}
	if m := codeLine.FindStringSubmatch(line); m != nil {
		c.UserCode = m[1]
	}
	return c.UserCode != ""
}

// ghRelease is gh's release archive for this system, whose names carry the
// version: gh_2.101.0_linux_arm64.tar.gz, gh_2.101.0_macOS_arm64.zip.
func ghRelease(ctx context.Context, goos, goarch string) (string, string, error) {
	name, err := ghAsset(goos, goarch)
	if err != nil {
		return "", "", err
	}
	tag, err := binfetch.Latest(ctx, "cli/cli")
	if err != nil {
		return "", "", err
	}
	file := fmt.Sprintf(name, strings.TrimPrefix(tag, "v"))
	return binfetch.Base + "/cli/cli/releases/download/" + tag + "/" + file, "bin/gh", nil
}

// ghAsset is the release file name for a system, with %s for the version.
func ghAsset(goos, goarch string) (string, error) {
	if goarch != "arm64" && goarch != "amd64" {
		return "", fmt.Errorf("%w: gh on %s/%s", binfetch.ErrUnsupported, goos, goarch)
	}
	switch goos {
	case "linux":
		return "gh_%s_linux_" + goarch + ".tar.gz", nil
	case "darwin":
		return "gh_%s_macOS_" + goarch + ".zip", nil
	}
	return "", fmt.Errorf("%w: gh on %s/%s", binfetch.ErrUnsupported, goos, goarch)
}
