package connect

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/repogoapp/host/internal/clilogin"
	"github.com/repogoapp/host/internal/errkind"
	"github.com/repogoapp/host/internal/mcp"
)

// A CLI's own sign-in, run on this machine: a device code is a browser step
// that finishes by itself, a paste sign-in a field for the page's code, and a
// callback one a page whose localhost redirect the phone forwards here.

var errNotInstalled = errkind.New(errkind.Unavailable, "connect: not installed")

// cliStatus is a CLI's sign-in for project: the account the project picked
// when it has one, else the CLI's own login on this environment.
func (s *Service) cliStatus(ctx context.Context, kind, project string, c *Connection) error {
	install, ok := s.d.CLIs.Get(ctx, kind)
	if !ok {
		return ErrInvalid.Errorf("RepoGo doesn't know a CLI called %q", kind)
	}
	if !install.Installed {
		return errNotInstalled.Errorf("Install %s on this environment first", install.Name)
	}
	c.Destination, c.Status = install.Name+" on this environment", Missing
	if install.Authed {
		c.Status = Connected
		if a := install.Account; a != nil {
			c.Account = firstOf(a.Email, a.Login, a.Name)
		}
	}
	if project == "" {
		return nil
	}
	id, picked, found := s.d.CLIs.PickedAccount(ctx, kind, mcp.Root(project))
	switch {
	case id == "":
	case !found:
		c.Status, c.Account, c.Reason = Missing, "", pickGone(install.Name, id)
	case picked.SignedIn:
		c.Status, c.Account = Connected, firstOf(picked.Detail, picked.ID)
	default:
		c.Status, c.Account = Missing, ""
	}
	return nil
}

// pickGone is why a project's picked account can't be used: the CLI no
// longer has it.
func pickGone(cli, id string) string {
	return fmt.Sprintf("This project uses the %s account %s, which no longer exists. Pick another in RepoGo.", cli, id)
}

// stepAccountName asks a new account's name, for a CLI with several.
const stepAccountName = "account_name"

// startCLI signs a CLI in, or for one with several accounts, account names
// which: "again:<id>" signs that one in again, "another" adds one, asking
// its name first.
func (s *Service) startCLI(ctx context.Context, f *flow, account string) error {
	var c Connection
	if err := s.cliStatus(ctx, f.target.CLI, f.project, &c); err != nil {
		return err
	}
	f.Destination = c.Destination
	switch {
	case strings.HasPrefix(account, "again:"):
		return s.startCLILogin(ctx, f, func() (clilogin.Code, error) {
			return s.d.CLIs.AccountLoginStart(ctx, f.target.CLI, strings.TrimPrefix(account, "again:"), false)
		})
	case account == "another":
		// Signed in under a name of its own, then named once it's clear which
		// account it is.
		f.account, f.adding = "new-"+strings.ToLower(rand.Text()[:4]), true
		return s.startCLILogin(ctx, f, func() (clilogin.Code, error) {
			return s.d.CLIs.AccountLoginStart(ctx, f.target.CLI, f.account, true)
		})
	case account != "":
		return ErrInvalid.Errorf("account must be again:<id> or another")
	case c.Status == Connected:
		f.end(Succeeded, "", s.d.Now())
		return nil
	case c.Reason != "":
		return ErrInvalid.Errorf("%s", c.Reason)
	}
	// A project with an account picked signs that one in, not the CLI's own.
	if id, _, found := s.d.CLIs.PickedAccount(ctx, f.target.CLI, mcp.Root(f.project)); found {
		return s.startCLILogin(ctx, f, func() (clilogin.Code, error) {
			return s.d.CLIs.AccountLoginStart(ctx, f.target.CLI, id, false)
		})
	}
	return s.startCLILogin(ctx, f, func() (clilogin.Code, error) { return s.d.CLIs.LoginStart(ctx, f.target.CLI) })
}

// startCLILogin starts a CLI's sign-in and waits on the page it prints. The
// flow is stored first, so a CLI that finishes at once still ends it through
// CLIEnded.
func (s *Service) startCLILogin(ctx context.Context, f *flow, start func() (clilogin.Code, error)) error {
	s.mu.Lock()
	s.flows[f.ID] = f
	s.mu.Unlock()
	code, err := start()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		delete(s.flows, f.ID)
		return err
	}
	if f.State == Waiting {
		f.Step = cliStep(code)
	}
	return nil
}

// nameStep asks a new, signed-in account's name, saying which account it is
// and suggesting one.
func (s *Service) nameStep(ctx context.Context, f flow) *Step {
	detail, suggestion := s.d.CLIs.AccountWho(ctx, f.target.CLI, f.account)
	note := "Name this account: it's the profile a project's agents use."
	if detail != "" {
		note = "Signed in to " + detail + ". " + note
	}
	return &Step{ID: stepAccountName, Kind: StepPaste, Label: "Name", Value: suggestion, Note: note}
}

// nameCLIAccount renames a new account to the name given, and its project
// uses it from now on. A pick that fails is the step's error, not a success:
// the account keeps its new name, so trying again only picks it.
func (s *Service) nameCLIAccount(ctx context.Context, f flow, value string) error {
	name := strings.TrimSpace(value)
	if name != f.account {
		if err := s.d.CLIs.RenameAccount(ctx, f.target.CLI, f.account, name); err != nil {
			return err
		}
		s.mu.Lock()
		if live := s.flows[f.ID]; live != nil {
			live.account = name
		}
		s.mu.Unlock()
	}
	if f.project == "" {
		return nil
	}
	if err := s.d.CLIs.SelectAccount(ctx, f.target.CLI, mcp.Root(f.project), name); err != nil {
		return fmt.Errorf("signed in as %s, but this project couldn't switch to it: %w", name, err)
	}
	return nil
}

func cliStep(code clilogin.Code) *Step {
	switch code.Style {
	case clilogin.StylePaste:
		note := "Sign in on the page, then paste the code it shows."
		if code.UserCode != "" {
			// A session ID the page shows too (Firebase), to check it's this sign-in.
			note = "Check the page shows session " + code.UserCode + ", sign in, then paste the code it gives you."
		}
		return &Step{ID: "code", Kind: StepPaste, Label: "Code", URL: code.URL, Note: note}
	case clilogin.StyleCallback:
		return &Step{ID: stepSignIn, Kind: StepBrowser, URL: code.URL, Redirect: RedirectLocalhost}
	default:
		return &Step{ID: stepSignIn, Kind: StepBrowser, URL: code.URL, UserCode: code.UserCode}
	}
}

// discardAdded deletes the account a flow added that never signed in. It
// runs once the flow has ended, with no caller left to give a context.
func (s *Service) discardAdded(f flow) {
	if err := s.d.CLIs.DiscardAccount(context.Background(), f.target.CLI, f.account); err != nil {
		s.warn("connect: discard an account that didn't sign in", "cli", f.target.CLI, "err", err)
	}
}

// disconnectCLI signs the CLI out on this environment, for every project.
func (s *Service) disconnectCLI(ctx context.Context, f flow) error {
	res, err := s.d.CLIs.Logout(ctx, f.target.CLI)
	if err != nil || !res.OK {
		return errSignOut
	}
	return nil
}

// errSignOut is a sign-out the CLI refused, worded here for the same reason
// as errSignIn.
var errSignOut = errkind.New(errkind.Unavailable, "The sign-out didn't finish. Try again.")

// CLIEnded is told when a CLI's sign-in ends, however it was started, and ends
// every flow waiting on it: the CLI runs one sign-in for all of them.
func (s *Service) CLIEnded(kind string, ok bool) {
	s.mu.Lock()
	var ended []Flow
	var naming, discard []flow
	for _, f := range s.flows {
		if f.target.Kind != KindCLI || f.target.CLI != kind || f.State != Waiting {
			continue
		}
		if f.adding {
			switch {
			case !ok:
				discard = append(discard, *f)
			case f.working || (f.Step != nil && f.Step.ID == stepAccountName):
				// A pasted code's submit asks the name itself.
				continue
			default:
				naming = append(naming, *f)
				continue
			}
		}
		f.working = false
		if ok {
			f.end(Succeeded, "", s.d.Now())
		} else {
			f.end(Failed, errSignIn.Error(), s.d.Now())
		}
		ended = append(ended, f.Flow)
	}
	s.mu.Unlock()
	for _, f := range discard {
		s.discardAdded(f)
	}
	// One signed in on its page (no code to paste) is named here.
	for _, f := range naming {
		step := s.nameStep(context.Background(), f)
		s.mu.Lock()
		live := s.flows[f.ID]
		moved := live != nil && live.State == Waiting
		if moved {
			live.Step, live.working, live.signedIn = step, false, true
		}
		var out Flow
		if moved {
			out = live.Flow
		}
		s.mu.Unlock()
		if moved {
			s.changed(out)
		}
	}
	for _, f := range ended {
		s.changed(f)
	}
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
