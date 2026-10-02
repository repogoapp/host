//go:build !aigen

package ai

// Without -tags aigen the binary carries no model and Model returns ErrNotBundled.
var (
	bundleVersion string
	bundleServer  []byte
	bundleModel   []byte
)
