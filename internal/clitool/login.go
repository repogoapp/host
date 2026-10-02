package clitool

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/repogo/host/internal/clilogin"
	"github.com/repogo/host/internal/errkind"
)

// Device codes and authorize links last 15 minutes or less, and no CLI is
// worth keeping past that.
const loginTimeout = 15 * time.Minute

var (
	ErrUnknownTool  = errkind.New(errkind.Invalid, "clitool: unknown tool")
	ErrNotInstalled = errkind.New(errkind.Unavailable, "clitool: install it before signing in")
)

// LoginStart runs the CLI's own sign-in and returns the page for the phone to
// open. A second call while one is waiting returns the same page.
func (in *Inventory) LoginStart(ctx context.Context, kind string) (clilogin.Code, error) {
	t, ok := in.Lookup(kind)
	if !ok || t.Spec.LoginRead == nil {
		return clilogin.Code{}, fmt.Errorf("%w %q", ErrUnknownTool, kind)
	}
	home, _ := os.UserHomeDir()
	path := FindBinary(t.Spec, home)
	if path == "" {
		return clilogin.Code{}, ErrNotInstalled
	}
	// The sign-in outlives this call, so its follow-up must too.
	after := context.WithoutCancel(ctx)
	return in.logins.Start(kind, clilogin.Spec{
		Command: path,
		Args:    t.Spec.LoginArgs,
		Env:     t.Spec.LoginEnv,
		Timeout: loginTimeout,
		TTY:     t.Spec.LoginTTY,
		Style:   t.Spec.LoginStyle,
		Port:    t.Spec.LoginPort,
		Read:    t.Spec.LoginRead,
		OnEnd: func(ok bool) {
			if ok && t.Spec.AfterSignIn != nil {
				t.Spec.AfterSignIn(after, path)
			}
			in.changed()
		},
	})
}

// LoginComplete hands a waiting sign-in the code its page showed.
func (in *Inventory) LoginComplete(ctx context.Context, kind, code string) error {
	return in.logins.Complete(ctx, kind, code)
}

// Close stops every waiting sign-in, for the host's shutdown.
func (in *Inventory) Close() { in.logins.Close() }

// LoginCancel stops a waiting sign-in; nothing waiting is not an error.
func (in *Inventory) LoginCancel(kind string) { in.logins.Cancel(kind) }

// Logins is which CLIs have a sign-in waiting: no subprocess, so a phone can
// ask it every second while its sheet is up.
func (in *Inventory) Logins() []string {
	out := []string{}
	for _, t := range in.Tools {
		if in.logins.Pending(t.Kind) != nil {
			out = append(out, t.Kind)
		}
	}
	return out
}
