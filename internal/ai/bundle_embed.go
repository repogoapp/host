//go:build aigen

package ai

import _ "embed"

// Staged by scripts/ai-bundle.sh; git-ignored, so only a tagged build needs them.
var (
	//go:embed assets/version
	bundleVersion string
	//go:embed assets/llama-server.tar.gz
	bundleServer []byte
	//go:embed assets/model.gguf
	bundleModel []byte
)
