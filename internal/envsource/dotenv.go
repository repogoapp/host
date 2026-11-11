package envsource

import "strings"

// Parse reads dotenv text as v1's phone and web do, so a file means the same
// everywhere: `KEY=value`, optional `export `, `#` and blank lines skipped,
// trimmed, one pair of matching quotes stripped. One escape differs: \n in
// double quotes is a line break, as Next.js, Bun and Wrangler read it and as
// Connect writes a key of several lines (a .p8).
func Parse(text string) map[string]string {
	out := map[string]string{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			if value[0] == '"' {
				value = strings.ReplaceAll(value, `\n`, "\n")
			}
			value = value[1 : len(value)-1]
		}
		out[key] = value
	}
	return out
}
