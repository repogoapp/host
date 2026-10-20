package connect

import (
	"context"

	"github.com/repogoapp/host/internal/clilogin"
	"github.com/repogoapp/host/internal/errkind"
)

// A CLI's own sign-in, run on this machine: a device code is a browser step
// that finishes by itself, a paste sign-in a field for the page's code, and a
// callback one a page whose localhost redirect the phone forwards here.

var errNotInstalled = errkind.New(errkind.Unavailable, "connect: not installed")

func (s *Service) cliStatus(ctx context.Context, kind string, c *Connection) error {
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
	return nil
}

func (s *Service) startCLI(ctx context.Context, f *flow) error {
	var c Connection
	if err := s.cliStatus(ctx, f.target.CLI, &c); err != nil {
		return err
	}
	f.Destination = c.Destination
	if c.Status == Connected {
		f.end(Succeeded, "", s.d.Now())
		return nil
	}
	code, err := s.d.CLIs.LoginStart(ctx, f.target.CLI)
	if err != nil {
		return err
	}
	switch code.Style {
	case clilogin.StylePaste:
		f.Step = &Step{ID: "code", Kind: StepPaste, Label: "Code", URL: code.URL,
			Note: "Sign in on the page, then paste the code it shows."}
	case clilogin.StyleCallback:
		f.Step = &Step{ID: "sign_in", Kind: StepBrowser, URL: code.URL, Redirect: RedirectLocalhost}
	default:
		f.Step = &Step{ID: "sign_in", Kind: StepBrowser, URL: code.URL, UserCode: code.UserCode}
	}
	return nil
}

// CLIEnded is told when a CLI's sign-in ends, however it was started, and ends
// the flow waiting on it.
func (s *Service) CLIEnded(kind string, ok bool) {
	s.mu.Lock()
	var waiting []*flow
	for _, f := range s.flows {
		if f.target.Kind == KindCLI && f.target.CLI == kind && f.State == Waiting {
			waiting = append(waiting, f)
		}
	}
	s.mu.Unlock()
	for _, f := range waiting {
		var err error
		if !ok {
			err = ErrInvalid.Errorf("The sign-in didn't finish. Try again.")
		}
		s.mu.Lock()
		stepID := ""
		if f.Step != nil {
			stepID = f.Step.ID
		}
		s.mu.Unlock()
		s.advance(f, stepID, nil, err)
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
