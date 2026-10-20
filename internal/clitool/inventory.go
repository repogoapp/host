package clitool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/apphome"
	"github.com/repogoapp/host/internal/binfetch"
	"github.com/repogoapp/host/internal/clilogin"
	"github.com/repogoapp/host/internal/par"
)

// Inventory answers which CLIs are installed on this machine, how old they
// are, what would update them and who they are signed in as. A settings
// screen asks it, so it may be slow and reach the network.
type Inventory struct {
	// Every CLI this host knows, in report order: agents, gh, cloud CLIs.
	Tools []Tool

	mu       sync.Mutex
	latest   map[string]latestEntry
	versions map[string]versionEntry

	// One update at a time: two concurrent `npm install -g` runs fight over the
	// same global prefix and the loser reports the package as broken.
	updateMu sync.Mutex

	// Sign-ins waiting on a person at the provider's page, by kind.
	logins   clilogin.Logins
	onChange func()
	// loginEnded hears which sign-in ended and how, set once at assembly.
	loginEnded func(kind string, ok bool)
	log        *slog.Logger
}

// OnLoginEnd tells f each sign-in's end by kind, however it was started.
func (in *Inventory) OnLoginEnd(f func(kind string, ok bool)) { in.loginEnded = f }

// NewInventory is an empty inventory; the host adds each tool. changed is
// told when a sign-in ends, however it ended, and when an install or update
// finishes.
func NewInventory(changed func(), log *slog.Logger) *Inventory {
	return &Inventory{latest: map[string]latestEntry{}, versions: map[string]versionEntry{}, onChange: changed, logins: clilogin.Logins{Log: log}, log: log}
}

// changed tells the host what a CLI has or offers moved; nil in tests.
func (in *Inventory) changed() {
	if in.onChange != nil {
		in.onChange()
	}
}

type latestEntry struct {
	version   string
	expiresAt time.Time
}

const (
	probeTimeout = 3 * time.Second

	// The registry is a nice-to-have: without it every install just reports an
	// unknown status, which is a worse screen but not a broken one. So it gets
	// a short leash.
	latestTimeout = 4 * time.Second
	latestTTL     = time.Hour

	// Generous because it is someone else's package manager doing the work, and
	// a cold npm cache on a slow connection is genuinely minutes.
	updateTimeout = 5 * time.Minute
)

// Install is one CLI as the phone shows it: whether it is here, how old,
// whether it updates, and who it is signed in as.
type Install struct {
	Kind string `json:"kind"`
	Name string `json:"name"`

	// The Environment section it shows in, and what the host can do with it;
	// see Tool.
	Category     string   `json:"category"`
	Capabilities []string `json:"capabilities" wire:"array"`

	// The binary looked for on PATH, which an uninstall names.
	Command   string `json:"command"`
	Installed bool   `json:"installed"`
	Version   string `json:"version,omitempty"`

	// Whether the CLI has credentials. Installed-but-signed-out is the failure
	// that presents as "it does nothing", and it is invisible without this.
	Authed bool `json:"authed"`

	// Who it is signed in as; nil when signed in with an API key or unknown.
	Account *Account `json:"account,omitempty"`

	// LoginPending is a sign-in waiting on the user, so a screen opened
	// mid-sign-in shows it rather than a second Sign in button.
	LoginPending *clilogin.Code `json:"login_pending,omitempty"`

	Latest string `json:"latest,omitempty"`

	// current | behind | unknown. Unknown covers both "no network" and "we
	// could not parse a version", because the client does the same thing with
	// either: show the version it has and offer no update.
	Status string `json:"status"`

	CanUpdate bool `json:"can_update"`
}

// found is one probe: the Install the phone gets, and what updating,
// uninstalling and signing out read from the binary on disk.
type found struct {
	Install
	path string
	// npm | bun | pnpm | homebrew | native | unknown. How it was installed
	// decides how it updates.
	manager       string
	updateCommand string
	// Where the credentials were found, for a sign-out that left some behind.
	authSource string
}

// Account is the person a CLI is signed in as. Never a token.
type Account struct {
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`

	// Login is the account's handle where the provider has one, as GitHub does.
	Login string `json:"login,omitempty"`
}

// AccountKey hashes a provider's user and organization ids, so the ids never
// leave the host. Both, because a Team organization is shared and its limits
// are per person.
func AccountKey(kind, user, org string) string {
	if user == "" || org == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(kind + "\x00" + user + "\x00" + org))
	return hex.EncodeToString(sum[:8])
}

// UpdateResult is one attempted install, update, uninstall or sign-out. What
// ran and its output go to the host's log.
type UpdateResult struct {
	Kind  string `json:"kind"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// List probes every known CLI. Probes run concurrently: each one is a `which`,
// a `--version` subprocess and possibly an HTTPS round trip, and doing two of
// them in series is twice the wait for no reason.
func (in *Inventory) List(ctx context.Context) []Install {
	return par.Map(in.Tools, 0, func(t Tool) Install { return in.probe(ctx, t).Install })
}

// Get probes one CLI.
func (in *Inventory) Get(ctx context.Context, kind string) (Install, bool) {
	f, ok := in.find(ctx, kind)
	return f.Install, ok
}

func (in *Inventory) find(ctx context.Context, kind string) (found, bool) {
	t, ok := in.Lookup(kind)
	if !ok {
		return found{}, false
	}
	return in.probe(ctx, t), true
}

// AccountKey is the key of the seat a CLI's files say it is signed in to, ""
// when they name none; it never leaves the host.
func (in *Inventory) AccountKey(kind string) string {
	t, ok := in.Lookup(kind)
	if !ok || t.Spec.Account == nil || t.AccountPath == "" {
		return ""
	}
	_, key := t.Spec.Account(t.AccountPath)
	return key
}

// Ready is whether a CLI is installed and signed in, so its account can be read.
func (in *Inventory) Ready(ctx context.Context, kind string) bool {
	install, ok := in.Get(ctx, kind)
	return ok && install.Installed && install.Authed
}

// Lookup is the tool a kind names, as it arrives off the wire.
func (in *Inventory) Lookup(kind string) (Tool, bool) {
	for _, t := range in.Tools {
		if t.Kind == kind {
			return t, true
		}
	}
	return Tool{}, false
}

func (in *Inventory) probe(ctx context.Context, t Tool) found {
	home, _ := os.UserHomeDir()
	s := t.Spec

	f := found{manager: "unknown", Install: Install{
		Kind:         t.Kind,
		Name:         t.Name,
		Category:     t.Category,
		Capabilities: append([]string{}, t.Capabilities...),
		Command:      s.Command,
		Status:       "unknown",
	}}
	i := &f.Install

	i.LoginPending = in.logins.Pending(t.Kind)
	path := FindBinary(s, home)
	if path == "" {
		// Not installed is a complete answer: there is no version to read, no
		// credentials that would matter, and the only useful action is an install.
		return f
	}
	i.Installed = true
	f.path = path
	// The version, the sign-in and the registry's latest are separate
	// subprocesses and a round trip, so they run together.
	var latest string
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); i.Version = in.version(ctx, path) }()
	go func() { defer wg.Done(); latest = in.latestVersion(ctx, s) }()
	if s.AuthProbe != nil {
		i.Authed, f.authSource, i.Account = s.AuthProbe(ctx, path)
	} else {
		i.Authed, f.authSource = probeAuth(ctx, s, t.Home)
	}
	wg.Wait()
	if i.Authed && s.Account != nil && t.AccountPath != "" {
		i.Account, _ = s.Account(t.AccountPath)
	}

	f.manager = managerOf(path, s, home)
	f.updateCommand = updateCommandOf(f.manager, s)
	i.CanUpdate = f.updateCommand != "" || (f.manager == "native" && s.Release != nil)
	if i.Authed && s.LogoutArgs != nil {
		i.Capabilities = append(i.Capabilities, CapabilityLogout)
	}
	if canUninstall(f.manager, s) {
		i.Capabilities = append(i.Capabilities, CapabilityUninstall)
	}

	// A version we could not read makes every comparison meaningless.
	if i.Version != "" {
		i.Latest = latest
	}
	if i.Version != "" && i.Latest != "" {
		if compareVersions(i.Version, i.Latest) < 0 {
			i.Status = "behind"
		} else {
			i.Status = "current"
		}
	}
	return f
}

// FindBinary is the CLI on PATH, else where its own installer puts it: the
// host's PATH was fixed when its service was installed, before any later
// install added a directory to the user's shell.
func FindBinary(s Spec, home string) string {
	if path, err := exec.LookPath(s.Command); err == nil {
		return path
	}
	return nativeBinary(home, s.NativePaths)
}

func probeAuth(ctx context.Context, s Spec, cfg string) (bool, string) {
	for _, name := range s.AuthEnv {
		if os.Getenv(name) != "" {
			return true, "env:" + name
		}
	}
	if cfg != "" {
		for _, rel := range s.AuthPaths {
			abs := filepath.Join(cfg, rel)
			st, err := os.Stat(abs)
			if err != nil {
				continue
			}
			// An empty credentials file is what a signed-out CLI leaves behind,
			// so size is part of the question, not a detail.
			if st.IsDir() || st.Size() > 0 {
				return true, abs
			}
		}
	}
	if s.Keychain != "" && runtime.GOOS == "darwin" && keychainHas(ctx, s.Keychain) {
		return true, "keychain:" + s.Keychain
	}
	return false, ""
}

func keychainHas(ctx context.Context, service string) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	// Without -w or -g it prints no secret; only the exit code is wanted.
	return exec.CommandContext(ctx, "security", "find-generic-password", "-s", service).Run() == nil
}

// managerOf works out how a binary got here from where it lives, resolving
// symlinks first: the link target lands inside the installing manager's tree.
func managerOf(path string, s Spec, home string) string {
	candidates := []string{path}
	if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
		candidates = append(candidates, real)
	}

	if home != "" {
		for _, rel := range s.NativePaths {
			native := filepath.Join(home, rel)
			for _, c := range candidates {
				if c == native {
					return "native"
				}
			}
		}
	}

	for _, c := range candidates {
		p := strings.ToLower(filepath.ToSlash(c))
		switch {
		case strings.Contains(p, "/.bun/"):
			return "bun"
		case strings.Contains(p, "/pnpm/"), strings.Contains(p, "/.pnpm/"):
			return "pnpm"
		case strings.Contains(p, "/cellar/"), strings.Contains(p, "/caskroom/"),
			strings.Contains(p, "/homebrew/"):
			return "homebrew"
		case strings.Contains(p, "/lib/node_modules/"), strings.Contains(p, "/node_modules/.bin/"),
			strings.Contains(p, "/npm/node_modules/"):
			return "npm"
		}
	}
	return "unknown"
}

func updateCommandOf(manager string, s Spec) string {
	switch manager {
	case "npm":
		return "npm install -g " + s.Pkg + "@latest"
	case "bun":
		return "bun add -g " + s.Pkg + "@latest"
	case "pnpm":
		return "pnpm add -g " + s.Pkg + "@latest"
	case "homebrew":
		if s.Formula == "" {
			return ""
		}
		return "brew upgrade " + s.Formula
	case "native":
		return strings.Join(s.NativeUpdate, " ")
	}
	// Deliberately nothing for an unknown manager. Guessing npm for a binary
	// that came from somewhere else installs a second copy that PATH may not
	// even reach, and the user is left with two versions and no explanation.
	return ""
}

// Update runs one install's update command and re-probes it. Reported rather
// than returned as an error so one failing package manager cannot hide the
// others.
func (in *Inventory) Update(ctx context.Context, kind string) UpdateResult {
	res := UpdateResult{Kind: kind}

	current, known := in.find(ctx, kind)
	if !known {
		res.Error = "unknown tool " + kind
		return res
	}
	if !current.Installed {
		res.Error = kind + " is not installed"
		return res
	}
	if !current.CanUpdate {
		res.Error = "nothing here knows how to update a " + current.manager + " install of " + kind
		return res
	}
	// Told even when it failed: a half-run package manager may have moved the CLI.
	defer in.changed()
	t, _ := in.Lookup(kind)
	if current.updateCommand == "" {
		return in.fetchRelease(ctx, t, res)
	}
	// npm can't replace a package in a folder this user can't write (a sudo
	// install); it fails with only an exit code, so say what to run instead.
	if dir := npmGlobalDir(current.path); current.manager == "npm" && dir != "" && !writable(dir) {
		npm := filepath.Join(dir, "..", "..", "bin", "npm")
		res.Error = current.Name + " is installed in " + dir + ", which only sudo can change. Update it in Terminal: sudo " +
			filepath.Clean(npm) + " install -g " + t.Spec.Pkg + "@latest"
		return res
	}
	fields := strings.Fields(current.updateCommand)
	// A self-updating CLI may be off the host's PATH (see FindBinary).
	if fields[0] == current.Command {
		fields[0] = current.path
	}
	res.OK = in.runLocked(ctx, &res, fields[0], fields[1:]...)
	return res
}

// Install puts a missing CLI on this machine with a fixed command, and shares
// Update's lock because both write the same package manager's prefix. A failed
// install is reported in the result; only an unknown kind is an error.
func (in *Inventory) Install(ctx context.Context, kind string) (UpdateResult, error) {
	res := UpdateResult{Kind: kind}

	current, known := in.Get(ctx, kind)
	if !known {
		return res, fmt.Errorf("%w %q", ErrUnknownTool, kind)
	}
	if current.Installed {
		res.OK = true
		return res, nil
	}
	defer in.changed()
	t, _ := in.Lookup(kind)
	command := InstallCommandOf(t.Spec, lookPath)
	if command == "" && t.Spec.Release != nil {
		return in.fetchRelease(ctx, t, res), nil
	}
	if command == "" {
		res.Error = "installing " + current.Name + " needs Node (npm) or Homebrew on this environment"
		return res, nil
	}

	if !in.runLocked(ctx, &res, "/bin/sh", "-c", command) {
		return res, nil
	}
	if after, _ := in.Get(ctx, kind); !after.Installed {
		res.Error = "the installer finished but " + current.Command + " is still not on this environment"
		return res, nil
	}
	res.OK = true
	return res, nil
}

// runLocked runs a package manager under the update lock, recording any
// failure in res and its output tail in the log; false when it failed.
func (in *Inventory) runLocked(ctx context.Context, res *UpdateResult, name string, args ...string) bool {
	in.updateMu.Lock()
	defer in.updateMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		res.Error = err.Error()
		if line := failureLine(string(out)); line != "" {
			res.Error += ": " + line
		}
		in.ranFailed(res.Kind, append([]string{name}, args...), out, err)
		return false
	}
	return true
}

// ranFailed logs a failed command and the end of its output, where the error is.
func (in *Inventory) ranFailed(kind string, command []string, out []byte, err error) {
	in.log.Warn("clitool: command failed", "kind", kind, "ran", strings.Join(command, " "), "err", err, "output", tail(string(out), 40))
}

// failureLine is the line of a failed command's output that says why: the
// first "Error:" line, since npm follows it with a stack and advice, else the last.
func failureLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, line := range lines {
		if _, reason, ok := strings.Cut(line, "Error: "); ok {
			return strings.TrimSpace(reason)
		}
	}
	return strings.TrimSpace(strings.TrimPrefix(lines[len(lines)-1], "npm error"))
}

// npmGlobalDir is the node_modules folder an npm-installed binary resolves
// into, which a global update rewrites; "" when it isn't in one.
func npmGlobalDir(path string) string {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return ""
	}
	slashed := filepath.ToSlash(real)
	i := strings.Index(slashed, "/lib/node_modules/")
	if i < 0 {
		return ""
	}
	return filepath.FromSlash(slashed[:i+len("/lib/node_modules")])
}

func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".repogo-write-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

// fetchRelease installs or updates from the tool's release build, under the
// same lock as the package managers.
func (in *Inventory) fetchRelease(ctx context.Context, t Tool, res UpdateResult) UpdateResult {
	url, member, err := t.Spec.Release(ctx, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	in.updateMu.Lock()
	defer in.updateMu.Unlock()
	if _, err := binfetch.Install(ctx, url, member, t.Spec.Command); err != nil {
		res.Error = err.Error()
		in.log.Warn("clitool: download failed", "kind", t.Kind, "url", url, "err", err)
		return res
	}
	if after, _ := in.Get(ctx, t.Kind); !after.Installed {
		res.Error = "downloaded " + t.Spec.Command + " but it is still not on this environment"
		return res
	}
	res.OK = true
	return res
}

// ReadJSON decodes a CLI's own file; missing or unreadable is false.
func ReadJSON(path string, into any) bool {
	found, err := apphome.ReadJSON(path, into)
	return found && err == nil
}

func lookPath(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// InstallCommandOf prefers the tool's own installer, then npm, then brew.
func InstallCommandOf(s Spec, has func(string) bool) string {
	switch {
	case s.InstallScript != "":
		return s.InstallScript
	case s.Pkg != "" && has("npm"):
		return "npm install -g " + s.Pkg
	case s.Formula != "" && has("brew"):
		return "brew install " + s.Formula
	}
	return ""
}

// UpdateAll updates every install that is behind and knows how. Silent about
// the rest: a CLI that is already current has nothing to report, and one
// installed by hand has nothing this can do about it.
func (in *Inventory) UpdateAll(ctx context.Context) []UpdateResult {
	out := []UpdateResult{}
	for _, i := range in.List(ctx) {
		if i.Status == "behind" && i.CanUpdate {
			out = append(out, in.Update(ctx, i.Kind))
		}
	}
	return out
}

// tail keeps the end of a package manager's output. The end is where the error
// is; the beginning is progress bars.
func tail(s string, lines int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "\n")
	if len(parts) <= lines {
		return s
	}
	return strings.Join(parts[len(parts)-lines:], "\n")
}
