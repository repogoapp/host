// Package remotehost finds the other RepoGo hosts this user can reach over
// SSH: the connections they named in ~/.ssh/config, read only once they ask
// for the list. Only one they switched on is ever dialed, as `ssh <alias>`
// with their own username, key and host trust, and checked with `repogo
// status --json` on the other end. Nothing here listens or announces.
package remotehost

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/repogoapp/host/internal/hostinfo"
)

// Host is one SSH connection the user named, usable or not.
type Host struct {
	// ID is its ~/.ssh/config alias: what agents and devices name it by, and
	// what SSH dials.
	ID string `json:"id"`
	// HostID and Version are what `repogo status` answered, empty until it did.
	HostID  string `json:"host_id,omitempty"`
	Version string `json:"version,omitempty"`
	State   State  `json:"state"`
	// Reason says, for a host that isn't Ready, what the user can do.
	Reason string `json:"reason,omitempty"`
	// Address is the HostName the file gives the alias, shown so the user
	// can tell which machine it is. A hint: SSH decides where the alias goes.
	Address string `json:"address,omitempty"`
	// Authorized is whether the user switched it on (Service.Authorize): this
	// host may dial it, and agents' calls may go to it. State says whether it
	// takes a call now. Set as a list is answered.
	Authorized bool `json:"authorized"`
}

// State is whether a host can run RepoGo's tools from here, and if not, why.
type State string

const (
	// Off is a host the user hasn't switched on: named, never dialed.
	Off          State = "off"
	Ready        State = "ready"
	Offline      State = "offline"
	NoKey        State = "no_key"
	NotInstalled State = "not_installed"
	NotRunning   State = "not_running"
	TooOld       State = "too_old"
	// Self is a name that reaches this host itself.
	Self State = "self"
)

// checkTimeout bounds one host's SSH call, login included.
const checkTimeout = 5 * time.Second

// Deps is how discovery reads this machine and reaches others; System is the real one.
type Deps struct {
	// Self is this host's ID: a name that reaches it is marked Self.
	Self string
	// SSHConfig is ~/.ssh/config's contents, read only; empty when there is
	// none. Called only once the user asked for the list.
	SSHConfig func() []byte
	// Run runs command on alias over SSH, never prompting: its output and
	// exit code, -1 when it didn't finish.
	Run func(ctx context.Context, alias, command string) (stdout, stderr []byte, code int)
	// Send is Run for a call: stdin on the command, over a connection kept
	// open between calls (ControlMaster).
	Send func(ctx context.Context, alias, command string, stdin []byte) (stdout, stderr []byte, code int)
	// Close ends the connection Send keeps open to alias; nil keeps none.
	Close func(ctx context.Context, alias string)
	// AuthorizedPath is where the user's choices are saved (AuthorizedFile);
	// empty keeps them in memory.
	AuthorizedPath string
	// Changed tells every device the list or a switch changed.
	Changed func()
	// Log takes the debug lines a development host keeps while this is
	// tried: each look, switch and call. Nil logs nothing.
	Log *slog.Logger
}

// debug writes one development log line.
func (d Deps) debug(msg string, args ...any) {
	if d.Log != nil {
		d.Log.Debug(msg, args...)
	}
}

// candidate is one alias in ~/.ssh/config.
type candidate struct {
	id string
	// address is the HostName its block gives; empty when it gives none.
	address string
}

// Discover names every alias and checks the ones on reports the user
// switched on, all at once, so it takes as long as the slowest. The rest are
// Off and never dialed.
func Discover(ctx context.Context, d Deps, on func(alias string) bool) []Host {
	cands := parseSSHConfig(d.SSHConfig())
	found := make([]Host, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		if !on(c.id) {
			found[i] = Host{ID: c.id, Address: c.address, State: Off}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			began := time.Now()
			found[i] = check(ctx, d, c)
			d.debug("hosts: checked", "alias", c.id, "state", found[i].State, "reason", found[i].Reason,
				"ms", time.Since(began).Milliseconds())
		}()
	}
	wg.Wait()
	return found
}

// check asks one host for its status; how the call fails says what's missing.
func check(ctx context.Context, d Deps, c candidate) Host {
	h := Host{ID: c.id, Address: c.address}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	stdout, stderr, code := d.Run(ctx, c.id, statusCommand)
	if h.State, h.Reason = classify(code, string(stderr), c.id); h.State != Ready {
		return h
	}
	var status hostinfo.Status
	_ = json.Unmarshal(stdout, &status)
	// The other host wrote these: kept short and printable before they reach
	// an agent's context or a terminal.
	h.HostID, h.Version = clean(status.HostID, 64), clean(status.Version, 32)
	switch h.HostID {
	case "":
		h.Version, h.State, h.Reason = "", TooOld, "update RepoGo there"
	case d.Self:
		h.State, h.Reason = Self, "this is the environment you're on"
	}
	return h
}

// classify reads an SSH call's exit code and stderr: 255 is SSH's own
// failure, whose text is OpenSSH's; 127 the remote shell's command not
// found; ExitNotRunning the repogo CLI's. "unknown flag" is what a release
// from before `status --json` prints, which can't change now.
func classify(code int, stderr, alias string) (State, string) {
	switch {
	case code == 0:
		return Ready, ""
	case code == 255 && strings.Contains(stderr, "Permission denied"):
		// A wrong username and a key it doesn't accept read the same.
		return NoKey, "SSH sign-in failed; it works here once ssh " + alias + " connects without asking for a password"
	case code == 255 && strings.Contains(stderr, "Host key verification failed"):
		return NoKey, "run ssh " + alias + " once in a terminal to trust its host key"
	case code == 127:
		return NotInstalled, "RepoGo isn't installed"
	case code == ExitNotRunning:
		return NotRunning, "run repogo start there"
	case strings.Contains(stderr, "unknown flag"):
		return TooOld, "update RepoGo there"
	}
	return Offline, firstLine(stderr)
}

// maxConfig bounds what is read of ~/.ssh/config, and one line of it.
const maxConfig = 1 << 20

// codeHosts are where an alias points to push code, not to a computer: left
// out rather than listed as a connection with no shell.
var codeHosts = map[string]bool{
	"github.com": true, "ssh.github.com": true, "gist.github.com": true, "gitlab.com": true,
	"altssh.gitlab.com": true, "bitbucket.org": true, "altssh.bitbucket.org": true,
	"ssh.dev.azure.com": true, "vs-ssh.visualstudio.com": true,
}

// parseSSHConfig is the aliases ~/.ssh/config names, each once, in the
// file's order: the first name of each Host line that isn't a pattern, with
// the HostName its block gives. Nothing else is taken from the file, key
// paths included, and nothing in it is run: Include and Match aren't followed.
func parseSSHConfig(data []byte) []candidate {
	var out []candidate
	seen := map[string]bool{}
	// open is the alias whose block the lines belong to; -1 for a block that
	// names none, a pattern's or a Match's.
	open := -1
	lines := bufio.NewScanner(bytes.NewReader(data))
	// A line too long to be a setting ends the list there, with what came before.
	lines.Buffer(nil, maxConfig)
	for lines.Scan() {
		key, args := configLine(lines.Text())
		switch strings.ToLower(key) {
		case "host":
			open = -1
			name, ok := firstName(args)
			if !ok || seen[strings.ToLower(name)] {
				continue
			}
			seen[strings.ToLower(name)] = true
			out = append(out, candidate{id: name})
			open = len(out) - 1
		case "match":
			open = -1
		case "hostname":
			if open >= 0 && out[open].address == "" && len(args) > 0 {
				out[open].address = clean(args[0], 128)
			}
		}
	}
	// An alias with no HostName dials its own name.
	return slices.DeleteFunc(out, func(c candidate) bool {
		at := c.address
		if at == "" {
			at = c.id
		}
		return codeHosts[strings.ToLower(at)]
	})
}

// configLine is one ssh_config line as its keyword and arguments: spaces or
// tabs between them, an optional "=" after the keyword, quotes around an
// argument, and a comment from "#" on. Empty for a blank or comment line, and
// for one whose quotes don't close: it isn't guessed at.
func configLine(line string) (key string, args []string) {
	line = strings.TrimSpace(line)
	end := strings.IndexAny(line, " \t=")
	if end < 0 {
		end = len(line)
	}
	key = line[:end]
	if strings.HasPrefix(key, "#") {
		return "", nil
	}
	rest := strings.TrimPrefix(strings.TrimLeft(line[end:], " \t"), "=")
	var arg strings.Builder
	quoted, started := false, false
	for _, r := range rest {
		switch {
		case r == '"':
			quoted, started = !quoted, true
		case quoted:
			arg.WriteRune(r)
		case r == ' ' || r == '\t':
			if started {
				args = append(args, arg.String())
				arg.Reset()
				started = false
			}
		case r == '#' && !started:
			return key, args
		default:
			arg.WriteRune(r)
			started = true
		}
	}
	if quoted {
		return "", nil
	}
	if started {
		args = append(args, arg.String())
	}
	return key, args
}

// firstName is the first of a Host line's names that is one machine's alias:
// not a pattern or a negation, and nothing SSH would read as an option, a
// user@host, a URI or more than one word, which would dial somewhere else
// than the alias the user was shown.
func firstName(names []string) (string, bool) {
	for _, name := range names {
		if name != "" && !strings.ContainsAny(name, "*?! \t@:/\\'") && !strings.HasPrefix(name, "-") && name == clean(name, 128) {
			return name, true
		}
	}
	return "", false
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return clean(line, maxLine)
}

// maxLine bounds one line of another host's error text in an answer.
const maxLine = 200

// clean is text another host wrote, as it may reach an agent or a terminal:
// no control or format characters (escapes, newlines, bidi overrides), and
// at most max runes.
func clean(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		if n == max {
			b.WriteString("…")
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}
