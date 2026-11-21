package desktop

import (
	"reflect"
	"testing"
)

const dockPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>persistent-apps</key><array>
 <dict><key>tile-data</key><dict>
  <key>bundle-identifier</key><string>com.google.Chrome</string>
  <key>file-data</key><dict><key>_CFURLString</key><string>file:///Applications/Google%20Chrome.app/</string></dict>
  <key>file-label</key><string>Google Chrome</string>
 </dict></dict>
 <dict><key>tile-data</key><dict>
  <key>bundle-identifier</key><string>com.apple.Terminal</string>
  <key>file-label</key><string>Terminal</string>
  <key>file-mod-date</key><date>2026-11-01T00:00:00Z</date>
 </dict></dict>
</array>
<key>persistent-others</key><array/>
</dict></plist>`

func TestParseDock(t *testing.T) {
	got, err := parseDock([]byte(dockPlist))
	if err != nil {
		t.Fatal(err)
	}
	want := []App{
		{Bundle: "com.google.Chrome", Name: "Google Chrome"},
		{Bundle: "com.apple.Terminal", Name: "Terminal"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

// Each app is listed once, in the Dock's order: Finder, the pinned apps,
// then open apps that aren't pinned, windows or not. Untitled helpers drop
// once titles are readable.
func TestMerge(t *testing.T) {
	pinned := []App{{Bundle: "com.google.Chrome", Name: "Google Chrome"}}
	open := []openApp{
		{App: App{Bundle: "com.apple.Preview", Name: "Preview"}},
		{App: App{Bundle: "com.google.Chrome", Name: "Google Chrome"}},
	}
	windows := []rawWindow{
		{ID: 3, Bundle: "com.example.Notes", App: "Notes", Title: "Ideas", Space: 2},
		{ID: 1, Bundle: "com.google.Chrome", App: "Google Chrome", Title: "Docs", Space: 1},
		{ID: 2, Bundle: "com.google.Chrome", App: "Google Chrome", Space: 1},
	}
	apps := merge(pinned, open, windows)
	var order []string
	for _, a := range apps {
		order = append(order, a.Bundle)
	}
	want := []string{homeApp.Bundle, "com.google.Chrome", "com.apple.Preview", "com.example.Notes"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order %v", order)
	}
	if c := apps[1]; !c.Running || len(c.Windows) != 1 || c.Windows[0].ID != 1 {
		t.Fatalf("chrome %+v", c)
	}
	if p := apps[2]; !p.Running || len(p.Windows) != 0 {
		t.Fatalf("preview %+v", p)
	}
}

const lsappinfoList = ` 1) "loginwindow" ASN:0x0-0x2002: 
    bundleID="com.apple.loginwindow"
    pid = 402 type="UIElement" flavor=3 Version="3085.6.3"
 2) "Finder" ASN:0x0-0x11011: 
    bundleID="com.apple.finder"
    bundle path="/System/Library/CoreServices/Finder.app"
    pid = 623 type="Foreground" flavor=3 Version="26.6"
 3) "universalaccessd" ASN:0x0-0x8008: 
    bundleID=[ NULL ] 
    pid = 410 type="Foreground"
 4) "Google Chrome" ASN:0x0-0x259c59a: 
    bundleID="com.google.Chrome"
    pid = 9120 type="Foreground" flavor=3
 5) "Messages" ASN:0x0-0x393393: (in front) 
    bundleID="com.apple.MobileSMS"
    pid = 80846 token=[sess=100002 pid=80846 uid:501,501,501 g:20,20 pV:520047] type="Foreground" flavor=3
 6) "Notes" ASN:0x0-0x395395: 
    bundleID="com.apple.Notes"
    pid = 80900 !signalled type="Foreground" flavor=3
`

// Only Dock apps (Foreground, with a bundle ID) are open apps.
func TestParseOpenApps(t *testing.T) {
	got := parseOpenApps([]byte(lsappinfoList))
	if len(got) != 4 {
		t.Fatalf("got %+v", got)
	}
	if f := got[0]; f.Bundle != "com.apple.finder" || f.Name != "Finder" || f.pid != 623 || f.asn != "ASN:0x0-0x11011:" {
		t.Fatalf("finder %+v", f)
	}
	if c := got[1]; c.Bundle != "com.google.Chrome" || c.pid != 9120 {
		t.Fatalf("chrome %+v", c)
	}
	// macOS 27 puts a token between the pid and its type.
	if m := got[2]; m.Bundle != "com.apple.MobileSMS" || m.pid != 80846 || m.asn != "ASN:0x0-0x393393:" {
		t.Fatalf("messages %+v", m)
	}
	if n := got[3]; n.Bundle != "com.apple.Notes" || n.pid != 80900 {
		t.Fatalf("notes %+v", n)
	}
}
