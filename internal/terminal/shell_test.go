package terminal

import (
	"encoding/base64"
	"encoding/binary"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestShellArgs(t *testing.T) {
	if got := RunArgs("/bin/zsh", "npm run dev"); !slices.Equal(got, []string{"-l", "-c", "npm run dev"}) {
		t.Errorf("zsh: %q", got)
	}
	if got := loginArgs("/bin/zsh"); !slices.Equal(got, []string{"-l"}) {
		t.Errorf("zsh login: %q", got)
	}
	if got := RunArgs(`C:\Windows\System32\cmd.exe`, "npm run dev"); !slices.Equal(got, []string{"/d", "/s", "/c", "npm run dev"}) {
		t.Errorf("cmd: %q", got)
	}
	shell := `C:\Program Files\PowerShell\7\pwsh.exe`
	if got := loginArgs(shell); !slices.Contains(got, "RemoteSigned") {
		t.Errorf("pwsh login: %q", got)
	}
	got := RunArgs(shell, `echo "it's"`)
	if !slices.Contains(got, "RemoteSigned") || got[len(got)-2] != "-EncodedCommand" {
		t.Fatalf("pwsh: %q", got)
	}
	if script := decoded(t, got[len(got)-1]); !strings.Contains(script, "\necho \"it's\"\n") || !strings.Contains(script, "exit $global:LASTEXITCODE") {
		t.Errorf("pwsh script: %q", script)
	}
}

func decoded(t *testing.T, s string) string {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b)%2 != 0 {
		t.Fatalf("not UTF-16 base64: %v", err)
	}
	units := make([]uint16, len(b)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(units))
}
