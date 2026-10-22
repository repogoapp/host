package connect

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/repogoapp/host/internal/envsource"
	"github.com/repogoapp/host/internal/errkind"
)

// maxValue bounds one value; a key or token is far shorter.
const maxValue = 16 << 10

// errRefused is a file no value could be written to, so the flow ends.
var errRefused = errkind.New(errkind.Denied, "connect: refused")

// hasKey is whether path sets name to something.
func hasKey(path, name string) bool {
	b, err := readEnv(path)
	return err == nil && envsource.Parse(string(b))[name] != ""
}

// envLine is name=value as a dotenv file holds it, quoted when the value
// needs it. A value one line can't hold is refused, never escaped.
func envLine(name, value string) (string, error) {
	switch {
	case value == "":
		return "", ErrInvalid.Errorf("a value is required")
	case len(value) > maxValue:
		return "", ErrInvalid.Errorf("the value is too long")
	case strings.ContainsAny(value, "\n\r\x00"):
		return "", ErrInvalid.Errorf("the value must be one line")
	case !strings.ContainsAny(value, " \t#'\"=$`\\"):
		return name + "=" + value, nil
	case !strings.ContainsAny(value, "'"):
		return name + "='" + value + "'", nil
	case !strings.ContainsAny(value, `"$`+"`\\"):
		return name + `="` + value + `"`, nil
	}
	return "", ErrInvalid.Errorf("the value can't hold both kinds of quote")
}

// writeKey sets name's line in path, replacing the one that sets it or adding
// one, and keeps every other line. The file is replaced whole, owner-only.
func writeKey(path, name, line string) error {
	b, err := readEnv(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	lines := strings.Split(string(b), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	replaced := false
	for i, l := range lines {
		if setsKey(l, name) {
			// The last line setting a key wins, so a later one would undo this write.
			if replaced {
				lines[i] = "# " + l
				continue
			}
			lines[i], replaced = line, true
		}
	}
	if !replaced {
		lines = append(lines, line)
	}
	return writeFile(path, []byte(strings.Join(lines, "\n")+"\n"))
}

// removeKey drops every line that sets name from path and keeps the rest; a
// missing file or key is already removed.
func removeKey(path, name string) error {
	b, err := readEnv(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var kept []string
	removed := false
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if setsKey(l, name) {
			removed = true
			continue
		}
		kept = append(kept, l)
	}
	if !removed {
		return nil
	}
	out := strings.Join(kept, "\n")
	if out != "" {
		out += "\n"
	}
	return writeFile(path, []byte(out))
}

// setsKey is whether a line sets name, as envsource.Parse reads it.
func setsKey(line, name string) bool {
	line = strings.TrimPrefix(strings.TrimSpace(line), "export ")
	key, _, found := strings.Cut(line, "=")
	return found && strings.TrimSpace(key) == name
}

// readEnv reads a dotenv file that is a regular file, not a link to one.
func readEnv(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errRefused.Errorf("%s is not a regular file", filepath.Base(path))
	}
	if info.Size() > envsource.MaxBytes {
		return nil, errRefused.Errorf("%s is too large", filepath.Base(path))
	}
	return os.ReadFile(path)
}

// writeFile replaces path through a temporary file beside it, so a reader
// never sees half a file.
func writeFile(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".connect-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
