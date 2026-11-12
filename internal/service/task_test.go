package service

import (
	"strings"
	"testing"
	"unicode/utf16"
)

// The launcher runs the binary under the user's PATH, logs to the host's
// log, marks itself the service and starts the host again when it exits;
// a percent sign in a path stays literal.
func TestTheTaskLauncherRestartsTheHost(t *testing.T) {
	s := &Service{home: `C:\Users\jo`, log: `C:\Users\jo\.repogo\host.log`, path: `C:\Users\jo\.repogo\host-task.cmd`}
	got := s.launcher(`C:\Users\jo\.repogo\bin\repogo.exe`, `C:\Program Files\nodejs;C:\100%`)
	for _, want := range []string{
		`set "PATH=C:\Program Files\nodejs;C:\100%%"`,
		`set "REPOGO_SERVICE=` + label + `"`,
		`"C:\Users\jo\.repogo\bin\repogo.exe" serve >> "C:\Users\jo\.repogo\host.log" 2>&1`,
		"goto run",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("launcher lacks %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "\r\n") {
		t.Error("cmd scripts need CRLF line ends")
	}
	if !strings.Contains(got, program("windows", `C:\Users\jo\.repogo\bin\repogo.exe`)) {
		t.Error("Runs can't find the binary's line in the launcher")
	}
}

// The task starts at the user's logon, never times out or stops on battery,
// and runs the launcher; schtasks reads it as UTF-16 with a byte-order mark.
func TestTheTaskRunsTheLauncherAtLogon(t *testing.T) {
	s := &Service{home: `C:\Users\jo`, path: `C:\Users\jo\.repogo\host-task.cmd`}
	raw := s.taskXML(`PC\jo & co`)
	if raw[0] != 0xff || raw[1] != 0xfe {
		t.Fatal("no UTF-16 byte-order mark")
	}
	units := make([]uint16, 0, len(raw)/2-1)
	for i := 2; i+1 < len(raw); i += 2 {
		units = append(units, uint16(raw[i])|uint16(raw[i+1])<<8)
	}
	xml := string(utf16.Decode(units))
	for _, want := range []string{
		"<LogonTrigger><Enabled>true</Enabled><UserId>PC\\jo &amp; co</UserId>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>",
		"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
		`<Arguments>/d /c "C:\Users\jo\.repogo\host-task.cmd"</Arguments>`,
	} {
		if !strings.Contains(xml, want) {
			t.Errorf("task lacks %q", want)
		}
	}
}
