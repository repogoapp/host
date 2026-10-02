package service

import (
	"encoding/xml"
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
