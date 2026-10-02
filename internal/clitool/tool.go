// Package clitool is every CLI this host finds, installs, updates and signs
// in: the agents, gh and the cloud CLIs. Each is a Tool value; one Inventory
// runs them all.
package clitool

import (
	"cmp"
	"context"
	"os"
	"path/filepath"

	"github.com/repogo/host/internal/clilogin"
)

// Categories group the Environment screen's sections.
const (
	CategorySourceControl = "source_control"
	CategoryAgent         = "agent"
	CategoryCloud         = "cloud"
)

// Capabilities a tool has from its Spec. Agents add their own (run, models)
// beside these; the values are wire strings the app decodes.
const (
	CapabilityInstall = "install"
	CapabilityUpdate  = "update"
	CapabilityLogin   = "login"

	// Added per install by the inventory: sign-out when the CLI has one, and
	// uninstall when the host knows how the CLI got here.
	CapabilityLogout    = "logout"
	CapabilityUninstall = "uninstall"
)

// Tool is one CLI: who it is, where it shows, and how to find, install,
// update and sign it in.
type Tool struct {
	Kind string

	// Human name on every screen: "Claude", not "claude".
	Name string

	// Asset name the app draws for this tool.
	Icon string

	// Category is one of the Category constants.
	Category string

	// What this host can do with the tool, sent to the app, which gates its
	// rows on them.
	Capabilities []string

	// The CLI's config directory and the file naming its signed-in account,
	// resolved per host so tests can confine them.
	Home, AccountPath string

	Spec Spec
}

// Spec is everything tool-specific about finding and maintaining one CLI.
type Spec struct {
	// AuthProbe delegates credential checks to a CLI that exposes status.
	AuthProbe func(ctx context.Context, path string) (bool, string, *Account)

	Command string

	// npm package, which is also the registry key for "what is the latest".
	Pkg string

	// GitHub repository whose latest release is the latest version, for a
	// CLI that is not on npm.
	ReleaseRepo string

	// Homebrew formula, when the CLI ships one. Empty means a binary under a
	// Homebrew prefix is reported but not updated through brew.
	Formula string

	// Relative to the tool's config directory; first existing wins.
	AuthPaths []string

	// macOS keychain service to fall back to. Claude stores its credentials
	// there on a Mac and in a file everywhere else.
	Keychain string

	// A binary at one of these paths (relative to $HOME) came from the
	// tool's own installer or release build and updates through it.
	NativePaths  []string
	NativeUpdate []string

	// The tool's own installer, run by sh; empty installs with npm or brew.
	InstallScript string

	// The tool's release build for a system, fetched into ~/.local/bin when
	// there is no installer, npm or brew; it updates the same way.
	Release func(ctx context.Context, goos, goarch string) (url, member string, err error)

	// Reads the account out of the file Tool.AccountPath names.
	Account func(path string) *Account

	// The CLI's browser sign-in, and how to read its link out of the output.
	LoginArgs []string
	LoginEnv  []string
	LoginRead func(line string, c *clilogin.Code) bool
	// LoginStyle is how the sign-in ends: clilogin.StyleDevice (the default),
	// StylePaste or StyleCallback.
	LoginStyle string
	// LoginTTY signs in on a pseudo-terminal, for a CLI that refuses without one.
	LoginTTY bool
	// LoginPort is the localhost port a callback sign-in's CLI listens on.
	LoginPort int

	// AfterSignIn runs once a sign-in succeeds, before the phones are told.
	AfterSignIn func(ctx context.Context, path string)

	// The CLI's own sign-out, run with LoginEnv; nil when it has none.
	LogoutArgs []string
}

// PreferredExecutable is the binary a one-shot runs: the native install when
// there is one, else whatever PATH finds. The native build updates itself, while
// a package-manager copy left earlier on PATH can be too old to answer at all.
func PreferredExecutable(s Spec) string {
	home, _ := os.UserHomeDir()
	return cmp.Or(nativeBinary(home, s.NativePaths), s.Command)
}

// nativeBinary is the first executable at one of paths (relative to home), or "".
func nativeBinary(home string, paths []string) string {
	if home == "" {
		return ""
	}
	for _, rel := range paths {
		path := filepath.Join(home, rel)
		if st, err := os.Stat(path); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return path
		}
	}
	return ""
}
