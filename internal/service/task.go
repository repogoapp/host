package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/repogoapp/host/internal/proctree"
)

// On Windows the host is a Task Scheduler task, the nearest thing to a launch
// agent: it starts at logon, outside any console or SSH session (whose
// processes end with it), and starts and ends on demand. It runs a launcher,
// s.path, that sets PATH, appends the host's output to its log and starts it
// again whenever it exits, as systemd's Restart=always does.

// taskName is the task's name in Task Scheduler.
const taskName = `RepoGo\Host`

// launcher is the script the task runs: binary under path, restarted five
// seconds after it exits. ping waits because timeout needs a console, which
// a task doesn't have.
func (s *Service) launcher(binary, path string) string {
	lines := []string{
		"@echo off",
		`set "PATH=` + cmdEscape(path) + `"`,
		`set "REPOGO_SERVICE=` + label + `"`,
		`cd /d "` + cmdEscape(s.home) + `"`,
		":run",
		program("windows", binary) + ` >> "` + cmdEscape(s.log) + `" 2>&1`,
		"ping -n 6 127.0.0.1 > nul",
		"goto run",
	}
	return strings.Join(lines, "\r\n") + "\r\n"
}

// cmdEscape keeps cmd from expanding a percent sign inside a quoted value.
func cmdEscape(v string) string { return strings.ReplaceAll(v, "%", "%%") }

// taskXML is the task's definition: start at this user's logon, no time
// limit, kept running on battery, one instance, out of sight.
func (s *Service) taskXML(account string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>RepoGo host</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled><UserId>` + xmlEscape(account) + `</UserId></LogonTrigger></Triggers>
  <Principals><Principal id="Author"><UserId>` + xmlEscape(account) + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <StartWhenAvailable>true</StartWhenAvailable>
    <Hidden>true</Hidden>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author"><Exec><Command>cmd.exe</Command><Arguments>/d /c "` + xmlEscape(s.path) + `"</Arguments><WorkingDirectory>` + xmlEscape(s.home) + `</WorkingDirectory></Exec></Actions>
</Task>
`)
	return utf16LE(b.String())
}

// utf16LE is s as UTF-16 with a byte-order mark, the encoding schtasks
// reads a task's XML in.
func utf16LE(s string) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 0xfe})
	for _, u := range utf16.Encode([]rune(s)) {
		_ = binary.Write(&buf, binary.LittleEndian, u)
	}
	return buf.Bytes()
}

func (s *Service) installTask(ctx context.Context, binary string) error {
	if err := os.WriteFile(s.path, []byte(s.launcher(binary, os.Getenv("PATH"))), 0o600); err != nil {
		return err
	}
	u, err := user.Current()
	if err != nil {
		return err
	}
	xml := filepath.Join(filepath.Dir(s.path), "host-task.xml")
	if err := os.WriteFile(xml, s.taskXML(u.Username), 0o600); err != nil {
		return err
	}
	defer os.Remove(xml)
	if err := command(ctx, "schtasks", "/Create", "/TN", taskName, "/XML", xml, "/F"); err != nil {
		return fmt.Errorf("register the host's task: %w", err)
	}
	return nil
}

// taskRunning reports whether the task is running now.
func taskRunning(ctx context.Context) bool {
	out, err := outputOf(ctx, "schtasks", "/Query", "/TN", taskName, "/FO", "CSV", "/NH")
	return err == nil && strings.Contains(out, `"Running"`)
}

func startTask(ctx context.Context) error {
	if taskRunning(ctx) {
		return nil
	}
	return command(ctx, "schtasks", "/Run", "/TN", taskName)
}

// stopTask ends the task, then the launcher and the host. Ending a task
// ends only the process it started, the launcher, which leaves the host
// running orphaned; so the launcher (by its script) and every host (a
// repogo serve) are killed with their trees.
func (s *Service) stopTask(ctx context.Context) error {
	if taskRunning(ctx) {
		if err := command(ctx, "schtasks", "/End", "/TN", taskName); err != nil {
			return err
		}
	}
	pids, err := hostProcesses(ctx, s.path)
	if err != nil {
		return err
	}
	for _, pid := range pids {
		if pid != os.Getpid() {
			_ = proctree.Kill(pid)
		}
	}
	return nil
}

// hostProcesses is the launchers running script and the repogo serve
// processes, launchers first so none starts a host again meanwhile.
func hostProcesses(ctx context.Context, script string) ([]int, error) {
	// -Command takes no arguments of its own, so the script's path goes in
	// as a single-quoted literal.
	query := `$s = '` + strings.ReplaceAll(script, "'", "''") + `'; $all = Get-CimInstance Win32_Process; ` +
		`$all | Where-Object { $_.CommandLine -and $_.CommandLine.Contains($s) } | ForEach-Object { $_.ProcessId }; ` +
		`$all | Where-Object { $_.Name -eq 'repogo.exe' -and $_.CommandLine -match '\sserve(\s|$)' } | ForEach-Object { $_.ProcessId }`
	out, err := outputOf(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", query)
	if err != nil {
		return nil, fmt.Errorf("list the host's processes: %w", err)
	}
	var pids []int
	for _, field := range strings.Fields(out) {
		if pid, err := strconv.Atoi(field); err == nil && !slices.Contains(pids, pid) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func uninstallTask(ctx context.Context) error {
	return command(ctx, "schtasks", "/Delete", "/TN", taskName, "/F")
}
