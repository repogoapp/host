package desktop

import (
	"bufio"
	"bytes"
	"regexp"
	"strconv"
	"strings"
)

var (
	appLine = regexp.MustCompile(`^\s*\d+\) "(.*)" (ASN:\S+)`)
	// Other fields can sit between the pid and its type: `!signalled`, and
	// on macOS 27 `token=[…]`.
	pidLine = regexp.MustCompile(`^\s*pid = (\d+) .*?\btype="([^"]*)"`)
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
