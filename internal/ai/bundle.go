package ai

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/repogo/host/internal/binfetch"
)

type paths struct {
	server, model string
}

// extract unpacks the embedded llama-server and model into dir/<version> once,
// then removes other versions so an update doesn't leave 400 MB behind.
func extract(dir string) (paths, error) {
	version := strings.TrimSpace(bundleVersion)
	if version == "" {
		return paths{}, ErrNotBundled
	}
	dest := filepath.Join(dir, version)
	p := paths{server: filepath.Join(dest, "llama-server"), model: filepath.Join(dest, "model.gguf")}
	if _, err := os.Stat(p.model); err == nil {
		return p, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return paths{}, err
	}
	// Unpack beside dest and rename, so a half-written tree is never taken for
	// a finished one.
	tmp, err := os.MkdirTemp(dir, ".unpack-*")
	if err != nil {
		return paths{}, err
	}
	defer os.RemoveAll(tmp)
	if err := binfetch.Untar(bundleServer, tmp); err != nil {
		return paths{}, err
	}
	if err := os.WriteFile(filepath.Join(tmp, "model.gguf"), bundleModel, 0o644); err != nil {
		return paths{}, err
	}
	if err := os.RemoveAll(dest); err != nil {
		return paths{}, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return paths{}, err
	}
	old, _ := os.ReadDir(dir)
	for _, e := range old {
		if e.Name() != version && !strings.HasPrefix(e.Name(), ".unpack-") {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
	return p, nil
}
