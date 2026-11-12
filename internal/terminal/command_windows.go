package terminal

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// A Windows process can't be handed an extra pipe, so the command writes its
// code to a file the host names in REPOGO_EXIT_FILE, and the host looks for
// it. `finally` runs on Ctrl-C too, which reads as 130, as a shell's SIGINT.
const commandScript = "$global:LASTEXITCODE = 0; $__repogoRan = $false\n" +
	"try {\n%s\n$__repogoOK = $?; $__repogoRan = $true\n} finally {\n" +
	"$__repogoCode = if (-not $__repogoRan) { 130 } elseif ($__repogoOK) { 0 } elseif ($global:LASTEXITCODE) { $global:LASTEXITCODE } else { 1 }\n" +
	"Set-Content -NoNewline -LiteralPath $env:REPOGO_EXIT_FILE -Value $__repogoCode\n}"

// exitPoll is how often a tab's command is checked for its code.
const exitPoll = 300 * time.Millisecond

func commandProcess(shell, line string) (*exec.Cmd, *exitCode, error) {
	f, err := os.CreateTemp("", "repogo-exit-*")
	if err != nil {
		return nil, nil, err
	}
	path := f.Name()
	f.Close()
	os.Remove(path)

	var cmd *exec.Cmd
	switch kindOf(shell) {
	case powerShell:
		script := strings.Replace(commandScript, "%s", line, 1)
		cmd = exec.Command(shell, append(append([]string{}, powerShellArgs...), "-NoExit", "-EncodedCommand", encoded(script))...)
	default:
		// cmd.exe keeps the tab open after line, without its code.
		cmd = exec.Command(shell, "/d", "/s", "/k", line)
	}
	return cmd, &exitCode{
		env:     []string{"REPOGO_EXIT_FILE=" + path},
		started: func() {},
		wait: func(open func() bool) (int, bool) {
			defer os.Remove(path)
			for open() {
				if raw, err := os.ReadFile(path); err == nil && len(raw) > 0 {
					code, err := strconv.Atoi(strings.TrimSpace(string(raw)))
					return code, err == nil
				}
				time.Sleep(exitPoll)
			}
			return 0, false
		},
		abort: func() { os.Remove(path) },
	}, nil
}
