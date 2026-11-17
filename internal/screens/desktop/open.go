package desktop

import (
	"bufio"
	"bytes"
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// openApps lists the open apps the Dock shows (Launch Services'
// "Foreground" type), windows or not, and which is in front, from
// `lsappinfo`: no permission, and no run loop, which NSWorkspace's
// running apps need to stay current in a process like the host.
func openApps(ctx context.Context) ([]openApp, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	list, err := exec.CommandContext(ctx, "lsappinfo", "list").Output()
	if err != nil {
		return nil, "", err
	}
	front, _ := exec.CommandContext(ctx, "lsappinfo", "front").Output()
	return parseOpenApps(list), strings.TrimSpace(string(front)), nil
}

var (
	appLine = regexp.MustCompile(`^\s*\d+\) "(.*)" (ASN:\S+)`)
	pidLine = regexp.MustCompile(`^\s*pid = (\d+) type="([^"]*)"`)
)

// parseOpenApps reads `lsappinfo list`: a numbered name line per process,
// then its bundleID and its pid and type.
func parseOpenApps(list []byte) []openApp {
	var out []openApp
	var cur openApp
	lines := bufio.NewScanner(bytes.NewReader(list))
	lines.Buffer(make([]byte, 64<<10), 1<<20)
	for lines.Scan() {
		line := lines.Text()
		if m := appLine.FindStringSubmatch(line); m != nil {
			cur = openApp{App: App{Name: m[1]}, asn: m[2]}
			continue
		}
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), `bundleID="`); ok {
			cur.Bundle, _, _ = strings.Cut(v, `"`)
			continue
		}
		if m := pidLine.FindStringSubmatch(line); m != nil && m[2] == "Foreground" && cur.Bundle != "" {
			cur.pid, _ = strconv.Atoi(m[1])
			out = append(out, cur)
		}
	}
	return out
}
