package service

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A path with XML's special characters must stay one string in the plist,
// or launchd refuses the file and the host never starts.
func TestPlistEscapesItsValues(t *testing.T) {
	s := &Service{home: "/Users/a&b", log: "/Users/a&b/host.log"}
	body := s.unit("darwin", "/opt/<repogo>/bin", "/usr/bin:/a&b")
	if err := xml.Unmarshal([]byte(body), new(struct{})); err != nil {
		t.Fatalf("plist is not XML: %v\n%s", err, body)
	}
	for _, want := range []string{"/opt/&lt;repogo&gt;/bin", "/usr/bin:/a&amp;b", "/Users/a&amp;b/host.log"} {
		if !strings.Contains(body, want) {
			t.Errorf("plist lacks %q:\n%s", want, body)
		}
	}
}

// systemd expands % specifiers everywhere and $ in ExecStart; a literal one
// in a path must survive both.
func TestUnitEscapesSpecifiers(t *testing.T) {
	s := &Service{home: "/home/50%", log: "/home/50%/host.log"}
	body := s.unit("linux", "/opt/$HOME/repogo", "/usr/bin:/x%y")
	for _, want := range []string{
		`ExecStart="/opt/$$HOME/repogo" serve`,
		`WorkingDirectory="/home/50%%"`,
		`Environment="PATH=/usr/bin:/x%%y"`,
		`StandardOutput=append:/home/50%%/host.log`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("unit lacks %q:\n%s", want, body)
		}
	}
}

// A unit left by another copy of repogo, since moved or deleted, never starts;
// Runs tells start to reinstall it for the binary running now.
func TestRunsMatchesOnlyTheInstalledBinary(t *testing.T) {
	s := &Service{path: filepath.Join(t.TempDir(), "unit"), home: "/home/a", log: "/home/a/host.log"}
	if ok, err := s.Runs("/opt/repogo"); err != nil || ok {
		t.Fatalf("no unit: Runs = %v, %v; want false", ok, err)
	}
	if err := os.WriteFile(s.path, []byte(s.unit(runtime.GOOS, "/opt/repogo & co/repogo", "/usr/bin")), 0o600); err != nil {
		t.Fatal(err)
	}
	for binary, want := range map[string]bool{
		"/opt/repogo & co/repogo":   true,
		"/opt/repogo & co/repogo2":  false,
		"/tmp/go-build1/exe/repogo": false,
	} {
		if ok, err := s.Runs(binary); err != nil || ok != want {
			t.Errorf("Runs(%q) = %v, %v; want %v", binary, ok, err, want)
		}
	}
}
