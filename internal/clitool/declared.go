package clitool

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/repogoapp/host/internal/clilogin"
)

// Declared is a CLI described by data alone: how to start its sign-in, how to
// read the page (and code) it prints, and a command that says who is signed
// in. It signs in and out but is never installed or updated here; a CLI that
// needs more (an account file, a seat key, an installer) is a Go Tool.
type Declared struct {
	Kind, Name string
	Command    string
	// NativePaths are where its own installer puts it, relative to $HOME, for
	// a host whose PATH doesn't have it.
	NativePaths []string

	// LoginArgs start the sign-in; Style is how it ends (clilogin.StyleDevice,
	// StylePaste or StyleCallback), TTY and Port as on Spec.
	LoginArgs []string
	Style     string
	TTY       bool
	Port      int
	// URLPattern finds the page to open in a line of its output, CodePattern
	// a code to type there (its first group, else the match); DefaultURL is
	// the page when the CLI prints only a code.
	URLPattern, CodePattern, DefaultURL string

	// StatusArgs print who is signed in: exit 0 with output is signed in, the
	// output's first line the account, or with StatusPattern the first group
	// of the line it matches (none matching is signed out). Status, for a CLI
	// with no such command, says it instead. LogoutArgs sign out, when it can.
	StatusArgs, LogoutArgs []string
	StatusPattern          string
	Status                 func(ctx context.Context, path string) (bool, *Account)

	// Accounts, for a CLI that keeps several, lists them and picks one per
	// project.
	Accounts AccountsProvider
}

// DeclaredCLIs are the CLIs this host signs in from data, after the Go ones.
var DeclaredCLIs = []Declared{{
	Kind:        "gcloud",
	Name:        "Google Cloud",
	Command:     "gcloud",
	NativePaths: []string{"google-cloud-sdk/bin/gcloud"},
	LoginArgs:   []string{"auth", "login", "--no-launch-browser"},
	Style:       clilogin.StylePaste,
	URLPattern:  `https://accounts\.google\.com/\S+`,
	StatusArgs:  []string{"auth", "list", "--filter=status:ACTIVE", "--format=value(account)"},
	LogoutArgs:  []string{"auth", "revoke"},
}, {
	Kind:       "aws",
	Name:       "AWS",
	Command:    "aws",
	LoginArgs:  []string{"login", "--remote"},
	Style:      clilogin.StylePaste,
	URLPattern: `https://[a-z0-9-]+\.signin\.aws\.amazon\.com/\S+`,
	StatusArgs: []string{"sts", "get-caller-identity", "--query", "Arn", "--output", "text"},
	LogoutArgs: []string{"logout"},
	Accounts:   awsProfiles{},
}, {
	Kind:    "fly",
	Name:    "Fly",
	Command: "fly",
	// Approved on another device, Fly shows a code to paste back, which it
	// reads only on a terminal; approved on this one, it ends on its own.
	LoginArgs:  []string{"auth", "login"},
	Style:      clilogin.StylePaste,
	TTY:        true,
	URLPattern: `https://fly\.io/\S+`,
	StatusArgs: []string{"auth", "whoami"},
	LogoutArgs: []string{"auth", "logout"},
	Accounts:   flyAccounts{},
}, {
	Kind:    "stripe",
	Name:    "Stripe",
	Command: "stripe",
	// A pairing code to match on the dashboard; it finishes once confirmed.
	LoginArgs:   []string{"login"},
	Style:       clilogin.StyleDevice,
	URLPattern:  `https://dashboard\.stripe\.com/stripecli/confirm_auth\?[^\s(]+`,
	CodePattern: `pairing code is: ([a-z]+(?:-[a-z]+)+)`,
	LogoutArgs:  []string{"logout"},
	Status:      stripeStatus,
}, {
	Kind:    "firebase",
	Name:    "Firebase",
	Command: "firebase",
	// It signs in only on a terminal: a session ID, a page, then its code.
	LoginArgs:   []string{"login", "--no-localhost"},
	Style:       clilogin.StylePaste,
	TTY:         true,
	URLPattern:  `https://auth\.firebase\.tools/login\?\S+`,
	CodePattern: `^\s*([A-Z0-9]{5,8})\s*$`,
	// It exits 0 signed out too, saying so.
	StatusArgs:    []string{"login:list"},
	StatusPattern: `^Logged in as (\S+@\S+)`,
	LogoutArgs:    []string{"logout"},
}}

// statusTTL is how long a status command's answer stands: cards and
// connect_list ask on every draw, and a CLI's own status can take a second.
const statusTTL = 15 * time.Second

// Tool is the Declared CLI as the inventory runs it: sign-in only.
func (d Declared) Tool() Tool {
	urls := compile(d.URLPattern)
	codes := compile(d.CodePattern)
	status := &statusCache{args: d.StatusArgs, pattern: compile(d.StatusPattern), read: d.Status}
	return Tool{
		Kind:         d.Kind,
		Name:         d.Name,
		Category:     CategoryCloud,
		Capabilities: []string{CapabilityLogin},
		Spec: Spec{
			Command:     d.Command,
			NativePaths: d.NativePaths,
			LoginArgs:   d.LoginArgs,
			LoginStyle:  d.Style,
			LoginTTY:    d.TTY,
			LoginPort:   d.Port,
			LoginRead:   readLine(urls, codes, d.DefaultURL),
			AuthProbe:   status.probe,
			// A sign-in or out changes the answer at once.
			AfterSignIn:  func(context.Context, string) { status.clear() },
			LogoutArgs:   d.LogoutArgs,
			AfterSignOut: status.clear,
			Accounts:     d.Accounts,
		},
	}
}

func compile(pattern string) *regexp.Regexp {
	if pattern == "" {
		return nil
	}
	return regexp.MustCompile(pattern)
}

// readLine reads a sign-in's output line by line: done once it has the page
// and, when the CLI shows one, the code.
func readLine(urls, codes *regexp.Regexp, defaultURL string) func(string, *clilogin.Code) bool {
	return func(line string, c *clilogin.Code) bool {
		if c.URL == "" {
			c.URL = defaultURL
		}
		if urls != nil {
			if u := urls.FindString(line); u != "" {
				c.URL = u
			}
		}
		if codes != nil {
			if m := codes.FindStringSubmatch(line); m != nil {
				c.UserCode = m[len(m)-1]
			}
		}
		return c.URL != "" && (codes == nil || c.UserCode != "")
	}
}

// statusCache asks who is signed in at most once per statusTTL: read, or
// else the status command, with pattern finding the account in its output.
type statusCache struct {
	args    []string
	pattern *regexp.Regexp
	read    func(context.Context, string) (bool, *Account)

	mu      sync.Mutex
	at      time.Time
	authed  bool
	account *Account
}

func (s *statusCache) probe(ctx context.Context, path string) (bool, string, *Account) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.at) < statusTTL {
		return s.authed, "status", s.account
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	if s.read != nil {
		s.authed, s.account = s.read(ctx, path)
	} else {
		out, err := exec.CommandContext(ctx, path, s.args...).Output()
		who := s.who(string(out))
		s.authed, s.account = err == nil && who != "", nil
		if s.authed {
			s.account = accountOf(who)
		}
	}
	s.at = time.Now()
	return s.authed, "status", s.account
}

// who is the account a status command's output names: its first line, or
// pattern's first group in the first line it matches.
func (s *statusCache) who(out string) string {
	if s.pattern == nil {
		first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
		return strings.TrimSpace(first)
	}
	for _, line := range strings.Split(out, "\n") {
		if m := s.pattern.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			return m[len(m)-1]
		}
	}
	return ""
}

func (s *statusCache) clear() {
	s.mu.Lock()
	s.at = time.Time{}
	s.mu.Unlock()
}

// accountOf names the account a status line gives: an AWS identity read as
// a person reads it, an email, else a login.
func accountOf(line string) *Account {
	if strings.HasPrefix(line, "arn:") {
		return &Account{Login: awsWho(line)}
	}
	if strings.Contains(line, "@") {
		return &Account{Email: line}
	}
	return &Account{Login: line}
}
