package plugins

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
)

// bundled is the SDK's runtime, bundled into runtime/ before a release build.
// A dev build carries only the folder's README.
//
//go:embed runtime
var bundled embed.FS

// Unpack writes the bundled runtime under dir, in a folder named for its
// contents so a new release never runs an old copy, and returns its entry
// (run.js); "" when this binary carries no runtime.
func Unpack(dir string) (string, error) {
	run, err := bundled.ReadFile("runtime/run.js")
	if err != nil {
		return "", nil
	}
	worker, err := bundled.ReadFile("runtime/worker.js")
	if err != nil {
		return "", err
	}
	executor, err := bundled.ReadFile("runtime/workflow-executor.js")
	if err != nil {
		return "", err
	}
	contents := append(append(append([]byte{}, run...), worker...), executor...)
	sum := sha256.Sum256(contents)
	folder := filepath.Join(dir, hex.EncodeToString(sum[:6]))
	entry := filepath.Join(folder, "run.js")
	if _, err := os.Stat(entry); err == nil {
		return entry, nil
	}
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return "", err
	}
	// Executors first: run.js present means the folder is complete.
	if err := writeFile(filepath.Join(folder, "workflow-executor.js"), executor); err != nil {
		return "", err
	}
	if err := writeFile(filepath.Join(folder, "worker.js"), worker); err != nil {
		return "", err
	}
	if err := writeFile(entry, run); err != nil {
		return "", err
	}
	return entry, nil
}

// writeFile writes through a temporary name, so a crash never leaves half a file.
func writeFile(path string, data []byte) error {
	partial := path + ".partial"
	if err := os.WriteFile(partial, data, fs.FileMode(0o600)); err != nil {
		return err
	}
	return os.Rename(partial, path)
}
