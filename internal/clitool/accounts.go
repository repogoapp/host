package clitool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/apphome"
	"github.com/repogoapp/host/internal/clilogin"
	"github.com/repogoapp/host/internal/errkind"
)

// A CLI with several accounts (AWS profiles) keeps them itself; the host only
// lists them, signs one in or adds one through the CLI's own sign-in, and
// remembers which one each project uses, which its agents get as Env. A
// project uses one account per CLI; none picked is the CLI's own default.

// AccountsProvider is how one CLI's accounts are listed and used.
type AccountsProvider interface {
	// List is every account, with who each is; bin is the CLI's binary.
	List(ctx context.Context, bin string) []CLIAccount
	// LoginArgs sign the account id in, with LoginEnv added to the CLI's own
	// sign-in environment.
	LoginArgs(id string) []string
	LoginEnv(id string) []string
	// Prepare readies a new account id before its first sign-in.
	Prepare(ctx context.Context, bin, id string) error
	// Env is what a project's agents get to use the account.
	Env(id string) string
	// Remove signs the account out and deletes it from the CLI's config.
	Remove(ctx context.Context, bin, id string) error
	// Rename renames the account in the CLI's config.
	Rename(ctx context.Context, bin, from, to string) error
	// Who is who the account signs in as, and a name to suggest for it.
	Who(ctx context.Context, bin, id string) (detail, suggestion string)
	// Default is which of accounts the CLI uses on its own, with no account
	// picked: "" when none of them is.
	Default(ctx context.Context, bin string, accounts []CLIAccount) string
}

// CLIAccount is one account of a CLI, as the phone lists it.
type CLIAccount struct {
	ID string `json:"id"`
	// Detail says who it is ("372…49 · user/S3"), or why not.
	Detail   string `json:"detail"`
	SignedIn bool   `json:"signed_in"`
	// CanSignIn is an account the CLI signs in through a browser; one of
	// stored keys has nothing to sign in again.
	CanSignIn bool `json:"can_sign_in"`
	// On is the account this project uses.
	On bool `json:"on"`
	// Default is the account the CLI uses on its own: what a project with
	// none picked gets.
	Default bool `json:"default"`
}

// AccountListing is a CLI's accounts for one project. Supported is false for
// a CLI with one sign-in, whose Connect is its plain sign-in.
type AccountListing struct {
	Supported bool         `json:"supported"`
	Accounts  []CLIAccount `json:"accounts" wire:"array"`
}

// accountsTTL is how long a listing stands: each account's check can take
// seconds, and the phone asks on every open.
const accountsTTL = 30 * time.Second

var (
	ErrNoAccounts = errkind.New(errkind.Invalid, "clitool: this CLI has no accounts here")
	// An account name goes on the CLI's command line and in its config.
	accountName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
)

// accountState is what the inventory keeps for accounts: each CLI's last
// listing, and the file of which account each project uses.
type accountState struct {
	mu       sync.Mutex
	path     string
	picks    map[string]map[string]string // kind → project root → account id
	listings map[string]accountsEntry
}

type accountsEntry struct {
	accounts []CLIAccount
	at       time.Time
}

// OpenAccounts loads which account each project uses from path, 0600.
func (in *Inventory) OpenAccounts(path string) error {
	s := &accountState{path: path, picks: map[string]map[string]string{}, listings: map[string]accountsEntry{}}
	if _, err := apphome.ReadJSON(path, &s.picks); err != nil {
		return err
	}
	in.accounts = s
	return nil
}

// Accounts is kind's accounts, the one root uses marked On.
func (in *Inventory) Accounts(ctx context.Context, kind, root string) (AccountListing, error) {
	_, p, bin, err := in.accountsTool(kind)
	if err != nil {
		return AccountListing{}, err
	}
	s := in.accounts
	s.mu.Lock()
	entry, ok := s.listings[kind]
	s.mu.Unlock()
	if !ok || time.Since(entry.at) > accountsTTL {
		accounts := p.List(ctx, bin)
		if id := p.Default(ctx, bin, accounts); id != "" {
			for i := range accounts {
				accounts[i].Default = accounts[i].ID == id
			}
		}
		entry = accountsEntry{accounts: accounts, at: time.Now()}
		s.mu.Lock()
		s.listings[kind] = entry
		s.mu.Unlock()
	}
	s.mu.Lock()
	picked := s.picks[kind][root]
	s.mu.Unlock()
	out := slices.Clone(entry.accounts)
	for i := range out {
		out[i].On = out[i].ID == picked
	}
	return AccountListing{Supported: true, Accounts: out}, nil
}

// SelectAccount has root use kind's account id, "" for the CLI's default.
func (in *Inventory) SelectAccount(ctx context.Context, kind, root, id string) error {
	_, _, _, err := in.accountsTool(kind)
	if err != nil {
		return err
	}
	if root == "" {
		return refuse("project is required")
	}
	// Only an account the CLI has: an id becomes a profile name or a folder.
	if id != "" {
		listing, err := in.Accounts(ctx, kind, root)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(listing.Accounts, func(a CLIAccount) bool { return a.ID == id }) {
			return refuse("there's no account called %s", id)
		}
	}
	s := in.accounts
	s.mu.Lock()
	if s.picks[kind] == nil {
		s.picks[kind] = map[string]string{}
	}
	before, had := s.picks[kind][root]
	if id == "" {
		delete(s.picks[kind], root)
	} else {
		s.picks[kind][root] = id
	}
	err = apphome.WriteJSON(s.path, s.picks, 0o600)
	if err != nil {
		if had {
			s.picks[kind][root] = before
		} else {
			delete(s.picks[kind], root)
		}
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	in.announceAccounts(kind)
	return nil
}

// AccountEnv is what agents in root get from the accounts it picked.
func (in *Inventory) AccountEnv(root string) []string {
	s := in.accounts
	if s == nil || root == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var env []string
	for _, t := range in.Tools {
		if id := s.picks[t.Kind][root]; id != "" && t.Spec.Accounts != nil {
			if v := t.Spec.Accounts.Env(id); v != "" {
				env = append(env, v)
			}
		}
	}
	return env
}

// AccountLoginStart signs kind's account id in, adding it when add, through
// the CLI's own sign-in; it ends as LoginStart's does.
func (in *Inventory) AccountLoginStart(ctx context.Context, kind, id string, add bool) (clilogin.Code, error) {
	t, p, bin, err := in.accountsTool(kind)
	if err != nil {
		return clilogin.Code{}, err
	}
	if !accountName.MatchString(id) {
		return clilogin.Code{}, refuse("name it with letters, digits, dots, dashes or underscores")
	}
	listing, _ := in.Accounts(ctx, kind, "")
	has := slices.ContainsFunc(listing.Accounts, func(a CLIAccount) bool { return a.ID == id })
	switch {
	case add && has:
		return clilogin.Code{}, refuse("%s already has an account called %s", t.Name, id)
	case !add && !has:
		return clilogin.Code{}, refuse("there's no account called %s", id)
	case add:
		if err := p.Prepare(ctx, bin, id); err != nil {
			return clilogin.Code{}, fmt.Errorf("prepare %s: %w", id, err)
		}
	}
	return in.logins.Start(kind, clilogin.Spec{
		Command: bin,
		Owner:   id,
		Args:    p.LoginArgs(id),
		Env:     append(slices.Clone(t.Spec.LoginEnv), p.LoginEnv(id)...),
		Timeout: loginTimeout,
		TTY:     t.Spec.LoginTTY,
		Style:   t.Spec.LoginStyle,
		Read:    t.Spec.LoginRead,
		OnEnd: func(ok bool) {
			in.dropListing(kind)
			in.announceAccounts(kind)
			if in.loginEnded != nil {
				in.loginEnded(kind, ok)
			}
			in.changed()
		},
	})
}

// RemoveAccount deletes one of kind's accounts, signing it out or removing
// its stored keys, and every project that used it goes back to the CLI's
// default. The phone asks first, more firmly for keys, which can't be
// recovered here.
func (in *Inventory) RemoveAccount(ctx context.Context, kind, id string) error {
	_, p, bin, err := in.accountsTool(kind)
	if err != nil {
		return err
	}
	listing, err := in.Accounts(ctx, kind, "")
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(listing.Accounts, func(a CLIAccount) bool { return a.ID == id }) {
		return refuse("there's no account called %s", id)
	}
	return in.discardAccount(ctx, kind, p, bin, id)
}

// discardAccount deletes an account and its picks, checked or not: a new
// one whose sign-in never finished is discarded the same way.
func (in *Inventory) discardAccount(ctx context.Context, kind string, p AccountsProvider, bin, id string) error {
	if err := p.Remove(ctx, bin, id); err != nil {
		return err
	}
	in.dropListing(kind)
	err := in.repick(kind, id, "")
	in.announceAccounts(kind)
	return err
}

// DiscardAccount deletes a new account whose sign-in didn't finish.
func (in *Inventory) DiscardAccount(ctx context.Context, kind, id string) error {
	_, p, bin, err := in.accountsTool(kind)
	if err != nil {
		return err
	}
	return in.discardAccount(ctx, kind, p, bin, id)
}

// RenameAccount renames one of kind's accounts, and every project's pick of it.
func (in *Inventory) RenameAccount(ctx context.Context, kind, from, to string) error {
	t, p, bin, err := in.accountsTool(kind)
	if err != nil {
		return err
	}
	switch {
	case !accountName.MatchString(to):
		return refuse("name it with letters, digits, dots, dashes or underscores")
	case from == "default" || to == "default":
		return refuse("the default account keeps its name")
	case from == to:
		return nil
	}
	in.dropListing(kind)
	listing, err := in.Accounts(ctx, kind, "")
	if err != nil {
		return err
	}
	if slices.ContainsFunc(listing.Accounts, func(a CLIAccount) bool { return a.ID == to }) {
		return refuse("%s already has an account called %s", t.Name, to)
	}
	if !slices.ContainsFunc(listing.Accounts, func(a CLIAccount) bool { return a.ID == from }) {
		return refuse("there's no account called %s", from)
	}
	if err := p.Rename(ctx, bin, from, to); err != nil {
		return err
	}
	in.dropListing(kind)
	err = in.repick(kind, from, to)
	in.announceAccounts(kind)
	return err
}

// PickedAccount is the account id root picked for kind, "" for none, and
// that account as listed; found is false for a pick the CLI no longer has
// (a profile deleted outside RepoGo). A project with no pick lists nothing.
func (in *Inventory) PickedAccount(ctx context.Context, kind, root string) (id string, account CLIAccount, found bool) {
	if s := in.accounts; s != nil {
		s.mu.Lock()
		id = s.picks[kind][root]
		s.mu.Unlock()
	}
	if id == "" {
		return "", CLIAccount{}, false
	}
	listing, err := in.Accounts(ctx, kind, root)
	if err != nil {
		return id, CLIAccount{}, false
	}
	for _, a := range listing.Accounts {
		if a.ID == id {
			return id, a, true
		}
	}
	return id, CLIAccount{}, false
}

// AccountWho is who kind's account id signs in as, and a name to suggest.
func (in *Inventory) AccountWho(ctx context.Context, kind, id string) (detail, suggestion string) {
	_, p, bin, err := in.accountsTool(kind)
	if err != nil {
		return "", ""
	}
	return p.Who(ctx, bin, id)
}

// repick moves every project's pick of kind's account from to to, or to
// none when to is "".
func (in *Inventory) repick(kind, from, to string) error {
	s := in.accounts
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for root, id := range s.picks[kind] {
		if id != from {
			continue
		}
		if to == "" {
			delete(s.picks[kind], root)
		} else {
			s.picks[kind][root] = to
		}
		changed = true
	}
	if !changed {
		return nil
	}
	return apphome.WriteJSON(s.path, s.picks, 0o600)
}

// dropListing forgets kind's cached listing, as its accounts just changed;
// the caller announces the change once it's whole.
func (in *Inventory) dropListing(kind string) {
	if s := in.accounts; s != nil {
		s.mu.Lock()
		delete(s.listings, kind)
		s.mu.Unlock()
	}
}

func (in *Inventory) accountsTool(kind string) (Tool, AccountsProvider, string, error) {
	t, ok := in.Lookup(kind)
	if !ok || t.Spec.Accounts == nil || in.accounts == nil {
		return Tool{}, nil, "", ErrNoAccounts
	}
	home, _ := os.UserHomeDir()
	bin := FindBinary(t.Spec, home)
	if bin == "" {
		return Tool{}, nil, "", ErrNotInstalled
	}
	return t, t.Spec.Accounts, bin, nil
}

// refuse is a refusal a person reads as is: the Invalid kind, its message
// with no "invalid parameters:" before it.
func refuse(format string, args ...any) error {
	return errkind.New(errkind.Invalid, fmt.Sprintf(format, args...))
}

// accountCheckTimeout bounds one of a CLI's quick commands: listing its
// accounts or asking who one is, at most a network round trip.
const accountCheckTimeout = 8 * time.Second

// runCLI runs one of a CLI's own quick commands, with env added ("NAME="
// unsets NAME), bounded by accountCheckTimeout: each is at most one network
// round trip.
func runCLI(ctx context.Context, bin string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, accountCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	if env != nil {
		cmd.Env = withEnv(cmd.Environ(), env)
	}
	out, err := cmd.Output()
	return string(out), err
}

// withEnv is base with each of env set, or for "NAME=", taken out.
func withEnv(base, env []string) []string {
	out := slices.Clone(base)
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		out = slices.DeleteFunc(out, func(e string) bool { return strings.HasPrefix(e, name+"=") })
		if value != "" {
			out = append(out, kv)
		}
	}
	return out
}
