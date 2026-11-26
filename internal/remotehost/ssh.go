package remotehost

// How this machine reaches another over SSH: the options on every call, the
// command that finds the other host's binary, and the real Deps.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"

	"github.com/repogoapp/host/internal/apphome"
	"github.com/repogoapp/host/internal/jsonrpc"
)

// sshOptions are on every SSH call RepoGo makes to another host. The alias
// is dialed as the user's own SSH settings say: their username, key, jump
// host, connection helpers and trusted host keys. On top of those: never
// prompt, never trust a new host key or rewrite the known ones, and hand the
// other host nothing of the user's: no agent, X11, ports, tunnel or tickets.
var sshOptions = []string{
	"-T", "-o", "BatchMode=yes",
	"-o", "StrictHostKeyChecking=yes", "-o", "UpdateHostKeys=no", "-o", "CheckHostIP=no",
	"-o", "PermitLocalCommand=no", "-o", "RemoteCommand=none",
	"-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes",
	"-o", "Tunnel=no", "-o", "GSSAPIDelegateCredentials=no",
}

// sshDir is ~/.repogo/ssh, private to this user, which a test host's own
// REPOGO_HOME keeps apart; false when it can't be made.
func sshDir() (string, bool) {
	dir, err := apphome.Path("ssh")
	if err != nil || os.MkdirAll(dir, 0o700) != nil || os.Chmod(dir, 0o700) != nil {
		return "", false
	}
	return dir, true
}

// HostBinFile names, under ~/.repogo, the file a serving host writes its own
// executable's path to: a development host runs from elsewhere than the
// release's ~/.repogo/bin/repogo, and the check must ask the host that runs.
const HostBinFile = "host-bin"

// Repogo runs the other host's repogo command: the running host's own
// binary, else the release's. A non-interactive SSH session's PATH lacks
// ~/.repogo/bin, so paths are spelled out, and sh runs it whatever the
// login shell is. No exec: macOS sh answers 126, not 127, for a missing file.
func Repogo(args string) string {
	return `sh -c 'b=$(cat ~/.repogo/` + HostBinFile + ` 2>/dev/null); [ -x "$b" ] || b=~/.repogo/bin/repogo; "$b" ` + args + `'`
}

// ExitNotRunning is the repogo command's exit code when the host it talks to
// isn't serving: stopped, or between a restart's two halves. Another host
// reads it over SSH, so the wording of the error is free to change.
const ExitNotRunning = 3

// statusCommand runs on the other host.
var statusCommand = Repogo("status --json")

// System is discovery on this machine: home's ~/.ssh/config, and ssh in
// batch mode.
func System(self, home string) Deps {
	return Deps{
		Self: self,
		// The only file RepoGo opens in ~/.ssh, and only once the user asked
		// for the list: read into memory, never written or moved. It holds
		// settings, no keys; private keys and known_hosts are never opened,
		// only used by ssh itself.
		SSHConfig: func() []byte {
			file, err := os.Open(filepath.Join(home, ".ssh", "config"))
			if err != nil {
				return nil
			}
			defer file.Close()
			data, _ := io.ReadAll(io.LimitReader(file, maxConfig))
			return data
		},
		Run: func(ctx context.Context, alias, command string) ([]byte, []byte, int) {
			args := slices.Concat(sshOptions, unshared, []string{"-o", "ConnectTimeout=3", "--", alias, command})
			return runSSH(ctx, args, nil)
		},
		Send: func(ctx context.Context, alias, command string, stdin []byte) ([]byte, []byte, int) {
			opts, _ := shared()
			args := slices.Concat(sshOptions, opts, []string{"-o", "ConnectTimeout=5",
				"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=2", "--", alias, command})
			return runSSH(ctx, args, stdin)
		},
		Close: func(ctx context.Context, alias string) {
			if opts, kept := shared(); kept {
				runSSH(ctx, slices.Concat(sshOptions, opts, []string{"-O", "exit", "--", alias}), nil)
			}
		},
	}
}

// unshared keeps an SSH call off the user's own shared connection: with
// ControlMaster in their settings it would otherwise open or reuse theirs,
// with forwarding off.
var unshared = []string{"-o", "ControlMaster=no", "-o", "ControlPath=none"}

// shared is the options that keep one connection per host open between
// calls, so only the first logs in: the socket under ~/.repogo/ssh.
// Unshared, and not kept, when that folder can't be made or its path is too
// long for a Unix socket.
func shared() (opts []string, kept bool) {
	dir, ok := sshDir()
	if !ok {
		return unshared, false
	}
	// %C is 40 hex characters; macOS allows 104 bytes in a socket path.
	if len(dir)+1+40 >= 104 {
		return unshared, false
	}
	return []string{"-o", "ControlMaster=auto", "-o", `ControlPath="` + filepath.Join(dir, "%C") + `"`, "-o", "ControlPersist=10m"}, true
}

// maxAnswer bounds what one SSH call may print, as one RPC message is, and
// maxErrors what it may write as errors, which only a line of is shown.
const (
	maxAnswer = jsonrpc.MaxMessageBytes
	maxErrors = 64 << 10
)

// runSSH runs ssh with args, stdin on it when set: its output, stdout cut at
// maxAnswer, and its exit code, -1 when it didn't finish.
func runSSH(ctx context.Context, args []string, stdin []byte) ([]byte, []byte, int) {
	stdout := &capped{max: maxAnswer}
	stderr := &capped{max: maxErrors}
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return stdout.Bytes(), stderr.Bytes(), 0
	case errors.As(err, &exit) && ctx.Err() == nil:
		return stdout.Bytes(), stderr.Bytes(), exit.ExitCode()
	}
	return stdout.Bytes(), stderr.Bytes(), -1
}

// capped keeps the first max bytes written to it and drops the rest.
type capped struct {
	bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
