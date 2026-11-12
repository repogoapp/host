package terminal

import (
	"encoding/base64"
	"encoding/binary"
	"strings"
	"unicode/utf16"
)

// The shells the host starts, for a person or for one command: a Unix login
// shell takes -l and -c; PowerShell takes its command encoded, so no quote in
// it meets Windows' command-line rules, and runs RemoteSigned for its own
// process, since Windows' default refuses npm's npm.ps1. A machine policy
// still wins over it.

type shellKind int

const (
	unixShell shellKind = iota
	powerShell
	cmdShell
)

// kindOf reads the shell's name after either slash, since a Windows path
// may be read on any machine (in a test, here).
func kindOf(shell string) shellKind {
	switch strings.ToLower(shell[strings.LastIndexAny(shell, `/\`)+1:]) {
	case "pwsh.exe", "pwsh", "powershell.exe", "powershell":
		return powerShell
	case "cmd.exe", "cmd":
		return cmdShell
	}
	return unixShell
}

// powerShellArgs open every PowerShell the host starts.
var powerShellArgs = []string{"-NoLogo", "-ExecutionPolicy", "RemoteSigned"}

// loginArgs start shell for a person: a login shell, or PowerShell without
// its banner.
func loginArgs(shell string) []string {
	switch kindOf(shell) {
	case powerShell:
		return powerShellArgs
	case cmdShell:
		return nil
	}
	return []string{"-l"}
}

// RunArgs run line once in shell and exit with its code: a login shell's -c,
// or PowerShell exiting with the last native command's code when it failed.
func RunArgs(shell, line string) []string {
	switch kindOf(shell) {
	case powerShell:
		// Text, not the CLIXML it writes to a redirected stream, and no
		// progress bar, since an action's output goes to a log.
		script := "$ProgressPreference = 'SilentlyContinue'; $global:LASTEXITCODE = 0\n" + line + "\n" +
			"if ($?) { exit 0 } elseif ($global:LASTEXITCODE) { exit $global:LASTEXITCODE } else { exit 1 }"
		return append(append([]string{}, powerShellArgs...), "-NonInteractive", "-OutputFormat", "Text", "-EncodedCommand", encoded(script))
	case cmdShell:
		return []string{"/d", "/s", "/c", line}
	}
	return []string{"-l", "-c", line}
}

// encoded is script as PowerShell's -EncodedCommand takes it: UTF-16LE, base64.
func encoded(script string) string {
	units := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(units))
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[2*i:], u)
	}
	return base64.StdEncoding.EncodeToString(b)
}
