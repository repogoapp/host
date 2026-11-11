package clitool

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/repogoapp/host/internal/par"
)

// flyAccounts are Fly accounts, each signed in to a config folder of its own
// that FLY_CONFIG_DIR points at, so one never replaces another, nor the
// user's own ~/.fly. Fly keeps and refreshes the token in it; the host only
// points a project's agents at the folder.
type flyAccounts struct{}

// flyHome is where each account's folder is kept: ~/.repogo/cli-accounts/fly.
func flyHome() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".repogo", "cli-accounts", "fly")
}

func flyDir(id string) string { return filepath.Join(flyHome(), id) }

func flyEnv(id string) []string { return []string{"FLY_CONFIG_DIR=" + flyDir(id)} }

func (flyAccounts) List(ctx context.Context, bin string) []CLIAccount {
	entries, err := os.ReadDir(flyHome())
	if err != nil {
		return nil
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && accountName.MatchString(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return par.Map(ids, 0, func(id string) CLIAccount {
		a := CLIAccount{ID: id, CanSignIn: true, Detail: "Signed out"}
		if who := flyWhoami(ctx, bin, id); who != "" {
			a.SignedIn, a.Detail = true, who
		}
		return a
	})
}

// flyWhoami is the email the account's folder is signed in as, "" when none.
func flyWhoami(ctx context.Context, bin, id string) string {
	out, err := runCLI(ctx, bin, flyEnv(id), "auth", "whoami")
	who, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if err != nil || who == "" {
		return ""
	}
	return who
}

// Default is the account signed in as whoever ~/.fly is: the login fly uses
// with no folder named.
func (flyAccounts) Default(ctx context.Context, bin string, accounts []CLIAccount) string {
	// The host's own login, even run where a project's pick is set.
	out, err := runCLI(ctx, bin, []string{"FLY_CONFIG_DIR="}, "auth", "whoami")
	who, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	if err != nil || who == "" {
		return ""
	}
	for _, a := range accounts {
		if a.SignedIn && a.Detail == who {
			return a.ID
		}
	}
	return ""
}

func (flyAccounts) LoginArgs(string) []string { return []string{"auth", "login"} }

func (flyAccounts) LoginEnv(id string) []string { return flyEnv(id) }

func (flyAccounts) Prepare(_ context.Context, _, id string) error {
	return os.MkdirAll(flyDir(id), 0o700)
}

func (flyAccounts) Env(id string) string { return flyEnv(id)[0] }

// Remove signs the account's folder out, then deletes it.
func (flyAccounts) Remove(ctx context.Context, bin, id string) error {
	_, _ = runCLI(ctx, bin, flyEnv(id), "auth", "logout") // signed out already is fine
	return os.RemoveAll(flyDir(id))
}

func (flyAccounts) Rename(_ context.Context, _, from, to string) error {
	return os.Rename(flyDir(from), flyDir(to))
}

// notName is what an email's local part may not keep as an account name.
var notName = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// Who is the account's email, and its name before the @ as a suggestion.
func (flyAccounts) Who(ctx context.Context, bin, id string) (detail, suggestion string) {
	who := flyWhoami(ctx, bin, id)
	if who == "" {
		return "", ""
	}
	local, _, _ := strings.Cut(who, "@")
	if name := strings.Trim(notName.ReplaceAllString(local, "-"), "-."); accountName.MatchString(name) {
		suggestion = name
	}
	return who, suggestion
}
