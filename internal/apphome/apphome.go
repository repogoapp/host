// Package apphome locates the host's own state directory: ~/.repogo. The
// v1 Mac app writes there too (files.db, host/, builds/, …), so the host's
// file names stay clear of those.
package apphome

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EnvVar overrides the directory wholesale, for tests and for running two
// hosts side by side without them sharing a cache.
const EnvVar = "REPOGO_HOME"

// Dir is $REPOGO_HOME, or ~/.repogo.
func Dir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(EnvVar)); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".repogo"), nil
}

// Path joins parts onto Dir.
func Path(parts ...string) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{dir}, parts...)...), nil
}

// MkdirAll creates the directory holding Path(parts...) and returns that path.
// Callers that are about to open a file for writing want this one: the state
// directory does not exist on a fresh machine.
func MkdirAll(parts ...string) (string, error) {
	path, err := Path(parts...)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return path, nil
}

// WriteFile replaces path atomically with perm from its first byte, so a
// secret whose mode was widened is tightened again.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// WriteJSON is WriteFile of v as indented JSON.
func WriteJSON(path string, v any, perm os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFile(path, append(b, '\n'), perm)
}

// ReadJSON unmarshals path into v. found is false, with v untouched, when the
// file does not exist: no state yet is not an error.
func ReadJSON(path string, v any) (found bool, err error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return true, fmt.Errorf("read %s: %w", path, err)
	}
	return true, nil
}
